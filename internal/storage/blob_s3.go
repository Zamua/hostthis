package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3BlobStore is a content-addressed blob store over an S3-compatible bucket.
// Bytes live at <prefix>/<sha256[:2]>/<sha256>, the disk store's layout, so a
// migration between them is a key rename rather than a re-encoding.
//
// A blob is immutable and content-addressed, so two writers racing one sha
// write identical bytes and last-write-wins is correct; no placement, fencing
// or consensus is needed here.
//
// Like the disk store it does NOT satisfy service.BlobStore: the at-rest
// encoding belongs to CompressedBlobStore, and every wiring path goes through
// that wrapper.
type S3BlobStore struct {
	client *minio.Client
	bucket string
	prefix string
}

// S3BlobConfig is the connection and placement for an S3BlobStore.
type S3BlobConfig struct {
	Endpoint  string // host:port, no scheme
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
	UseSSL    bool
	// Prefix namespaces the blobs inside the bucket, so a bucket already
	// holding another system's objects can take these without collision.
	Prefix string
}

func NewS3BlobStore(cfg S3BlobConfig) (*S3BlobStore, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("blob s3: bucket required")
	}
	endpoint := strings.TrimPrefix(strings.TrimPrefix(cfg.Endpoint, "https://"), "http://")
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("blob s3: client: %w", err)
	}
	prefix := strings.Trim(cfg.Prefix, "/")
	if prefix == "" {
		prefix = "blob"
	}
	return &S3BlobStore{client: client, bucket: cfg.Bucket, prefix: prefix}, nil
}

func (s *S3BlobStore) key(sha string) string {
	return s.prefix + "/" + sha[:2] + "/" + sha
}

// Put streams r to the content-addressed key. size must be accurate when
// known: a negative length makes the client buffer a whole multipart part to
// discover it. Callers that genuinely do not know pass -1 and accept that cost.
func (s *S3BlobStore) Put(sha string, r io.Reader, size int64) error {
	if len(sha) < 2 {
		return errors.New("blob: sha too short")
	}
	_, err := s.client.PutObject(context.Background(), s.bucket, s.key(sha), r, size,
		minio.PutObjectOptions{ContentType: "application/octet-stream"})
	if err != nil {
		return fmt.Errorf("blob s3: put %s: %w", sha, err)
	}
	return nil
}

// GetReader streams the stored bytes and their length. The caller closes it.
func (s *S3BlobStore) GetReader(sha string) (io.ReadCloser, int64, error) {
	if len(sha) < 2 {
		return nil, 0, ErrNotFound
	}
	obj, err := s.client.GetObject(context.Background(), s.bucket, s.key(sha), minio.GetObjectOptions{})
	if err != nil {
		return nil, 0, fmt.Errorf("blob s3: get %s: %w", sha, err)
	}
	// GetObject is lazy: a missing key surfaces on the first Stat/Read, so the
	// not-found translation happens against Stat.
	info, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		if minio.ToErrorResponse(err).StatusCode == 404 {
			return nil, 0, ErrNotFound
		}
		return nil, 0, fmt.Errorf("blob s3: stat %s: %w", sha, err)
	}
	return obj, info.Size, nil
}

// Get reads a whole blob. Present for the five-method contract; the serving
// paths use GetReader, because resident memory must not track payload size.
func (s *S3BlobStore) Get(sha string) ([]byte, error) {
	rc, _, err := s.GetReader(sha)
	if err != nil {
		return nil, err
	}
	defer rc.Close() //nolint:errcheck
	return io.ReadAll(rc)
}

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

// S3BlobStore is the object store over an S3-compatible bucket. An object
// lives at <key> in the bucket; a legacy content-addressed object at
// <legacy prefix>/<sha[:2]>/<sha>.
//
// Like the disk store it does NOT satisfy service.BlobStore: the at-rest
// encoding belongs to CompressedBlobStore, and every wiring path goes through
// that wrapper.
type S3BlobStore struct {
	client       *minio.Client
	bucket       string
	legacyPrefix string
}

// S3BlobConfig is the connection and placement for an S3BlobStore.
type S3BlobConfig struct {
	Endpoint  string // host:port, no scheme
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
	UseSSL    bool
	// LegacyPrefix names the content-addressed namespace legacy entries are
	// read from. New objects never go there.
	LegacyPrefix string
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
	legacy := strings.Trim(cfg.LegacyPrefix, "/")
	if legacy == "" {
		legacy = "blob"
	}
	return &S3BlobStore{client: client, bucket: cfg.Bucket, legacyPrefix: legacy}, nil
}

// Put streams r to key. size must be accurate when known: a negative length
// makes the client buffer a whole multipart part to discover it.
func (s *S3BlobStore) Put(key string, r io.Reader, size int64) error {
	if err := checkKey(key); err != nil {
		return err
	}
	_, err := s.client.PutObject(context.Background(), s.bucket, key, r, size,
		minio.PutObjectOptions{ContentType: "application/octet-stream"})
	if err != nil {
		return fmt.Errorf("blob s3: put %s: %w", key, err)
	}
	return nil
}

// GetReader streams the stored bytes and their length. The caller closes it.
func (s *S3BlobStore) GetReader(key string) (io.ReadCloser, int64, error) {
	if err := checkKey(key); err != nil {
		return nil, 0, err
	}
	return s.get(key)
}

// GetLegacyReader streams a legacy content-addressed object.
func (s *S3BlobStore) GetLegacyReader(sha string) (io.ReadCloser, int64, error) {
	if err := checkSHA(sha); err != nil {
		return nil, 0, err
	}
	return s.get(s.legacyPrefix + "/" + sha[:2] + "/" + sha)
}

func (s *S3BlobStore) get(key string) (io.ReadCloser, int64, error) {
	obj, err := s.client.GetObject(context.Background(), s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, 0, fmt.Errorf("blob s3: get %s: %w", key, err)
	}
	// GetObject is lazy: a missing key surfaces on the first Stat/Read, so the
	// not-found translation happens against Stat.
	info, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		if minio.ToErrorResponse(err).StatusCode == 404 {
			return nil, 0, ErrNotFound
		}
		return nil, 0, fmt.Errorf("blob s3: stat %s: %w", key, err)
	}
	return obj, info.Size, nil
}

// DeletePrefix lists only prefix and removes what it finds. An upload's prefix
// is written once and never churned, so the listing is bounded by its file
// count rather than by the bucket's history.
func (s *S3BlobStore) DeletePrefix(prefix string) error {
	if err := checkPrefix(prefix); err != nil {
		return err
	}
	ctx := context.Background()
	listed := s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true})
	objects := make(chan minio.ObjectInfo)
	listErr := make(chan error, 1)
	go func() {
		// The listing is drained even after an error, so its producer is never
		// left blocked on a send.
		var first error
		for obj := range listed {
			if obj.Err != nil {
				if first == nil {
					first = obj.Err
				}
				continue
			}
			if first == nil {
				objects <- obj
			}
		}
		listErr <- first
		close(objects)
	}()
	var removeErr error
	for rerr := range s.client.RemoveObjects(ctx, s.bucket, objects, minio.RemoveObjectsOptions{}) {
		if removeErr == nil {
			removeErr = fmt.Errorf("blob s3: delete %s: %w", rerr.ObjectName, rerr.Err)
		}
	}
	if err := <-listErr; err != nil {
		return fmt.Errorf("blob s3: list %s: %w", prefix, err)
	}
	return removeErr
}

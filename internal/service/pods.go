package service

import (
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// PodLinkRepo stores owners' pod links (docs/SPEC.md "Pod storage").
type PodLinkRepo interface {
	ActivePodLink(owner string) (*domain.PodLink, error)
	StartPodLink(owner, webid, linkID, secretHash string, expires time.Time) error
	ClearPodLink(owner string) (bool, error)
}

// Pods links owners' pods and names the upload ids that store in them.
type Pods struct {
	Repo PodLinkRepo
	Now  func() time.Time
}

func NewPods(repo PodLinkRepo) *Pods { return &Pods{Repo: repo, Now: time.Now} }

// PodLinkStart is what the owner opens to finish linking.
type PodLinkStart struct {
	ID      string
	Secret  string
	Expires time.Time
}

func (p *Pods) Link(owner domain.Owner, webid string) (PodLinkStart, error) {
	parsed, err := domain.ParseWebID(webid)
	if err != nil {
		return PodLinkStart{}, err
	}
	start := PodLinkStart{
		ID:      domain.NewPodLinkID(),
		Secret:  domain.NewPodLinkSecret(),
		Expires: p.Now().Add(domain.PodLinkTTL),
	}
	if err := p.Repo.StartPodLink(owner.String(), parsed, start.ID, domain.PodLinkSecretHash(start.Secret), start.Expires); err != nil {
		return PodLinkStart{}, err
	}
	return start, nil
}

func (p *Pods) Status(owner domain.Owner) (*domain.PodLink, error) {
	return p.Repo.ActivePodLink(owner.String())
}

func (p *Pods) Unlink(owner domain.Owner) (bool, error) {
	return p.Repo.ClearPodLink(owner.String())
}

// UploadID mints the id of an owner's next upload: one naming their pod when
// they have linked one.
func (p *Pods) UploadID(owner domain.Owner) (string, error) {
	link, err := p.Repo.ActivePodLink(owner.String())
	if err != nil {
		return "", err
	}
	if link == nil || !domain.ValidPodLinkID(link.ID) {
		return domain.NewUploadID(), nil
	}
	return domain.NewPodUploadID(link.ID), nil
}

// UploadIDSource mints an upload id for an owner; nil mints bucket ids.
type UploadIDSource func(owner domain.Owner) (string, error)

func (f UploadIDSource) next(owner domain.Owner) (string, error) {
	if f == nil {
		return domain.NewUploadID(), nil
	}
	return f(owner)
}

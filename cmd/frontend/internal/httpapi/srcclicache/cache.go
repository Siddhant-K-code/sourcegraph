package srcclicache

import (
	"context"
	"sync"
	"time"

	"github.com/rogpeppe/go-internal/semver"
	"github.com/sourcegraph/log"

	"github.com/sourcegraph/sourcegraph/internal/extsvc/github"
	"github.com/sourcegraph/sourcegraph/internal/goroutine"
	"github.com/sourcegraph/sourcegraph/lib/errors"
)

type VersionCache interface {
	goroutine.BackgroundRoutine
	CurrentVersion(branch string) (string, error)
	UpdateNow()
}

type versionCache struct {
	client *github.V4Client
	logger log.Logger

	done   chan struct{}
	update chan struct{}
	ticker *time.Ticker

	mu       sync.RWMutex
	versions map[string]string

	owner string
	name  string
}

func NewVersionCache(client *github.V4Client, interval time.Duration, owner, name string) VersionCache {
	return &versionCache{
		client:   client,
		logger:   log.Scoped("VersionCache", "src-cli version cache"),
		done:     make(chan struct{}, 1),
		update:   make(chan struct{}),
		ticker:   time.NewTicker(interval),
		versions: map[string]string{},
	}
}

func (vc *versionCache) CurrentVersion(branch string) (string, error) {
	vc.mu.RLock()
	defer vc.mu.RUnlock()

	if version, ok := vc.versions[branch]; ok {
		return version, nil
	}
	return "", errors.Newf("no version for branch %s", branch)
}

func (vc *versionCache) Start() {
	ctx := context.Background()

	go func() {
		for {
			select {
			case <-vc.done:
				vc.logger.Debug("stopping updater goroutine")
				return
			case <-vc.ticker.C:
				vc.fetch(ctx)
			case <-vc.update:
				vc.fetch(ctx)
			}
		}
	}()
}

func (vc *versionCache) Stop() {
	vc.logger.Debug("stopping ticker")
	vc.ticker.Stop()
	vc.done <- struct{}{}
}

func (vc *versionCache) UpdateNow() {
	vc.update <- struct{}{}
}

func (vc *versionCache) fetch(ctx context.Context) {
	vc.logger.Debug("updater awoken; checking for new src-cli versions")

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	versions := map[string]string{}
	params := github.ReleasesParams{
		Name:  vc.name,
		Owner: vc.owner,
	}
	for {
		releases, err := vc.client.Releases(ctx, &params)
		if err != nil {
			vc.logger.Warn("error getting releases", log.Error(err))
			return
		}

		for _, release := range releases.Nodes {
			if release.IsDraft || release.IsPrerelease {
				continue
			}

			// Since we know the releases are in descending release order, we
			// don't have to do any version comparisons: we can simply use the
			// first release on the branch only and ignore the rest.
			branch := semver.MajorMinor(release.TagName)
			if _, found := versions[branch]; !found {
				versions[branch] = release.TagName
			}
		}

		if !releases.PageInfo.HasNextPage {
			break
		}

		params.After = releases.PageInfo.EndCursor
	}

	vc.mu.Lock()
	defer vc.mu.Unlock()

	vc.versions = versions
}

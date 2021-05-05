package resourcestore

import (
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const sleepTimeBeforeCleanup = 1 * time.Minute

// ResourceStore is a structure that saves information about a recently created resource.
// Resources can be added and retrieved from the store. A retrieval (Get) also removes the Resource from the store.
// The ResourceStore comes with a cleanup routine that loops through the resources and marks them as stale, or removes
// them if they're already stale, then sleeps for `timeout`.
// Thus, it takes between `timeout` and `2*timeout` for unrequested resources to be cleaned up.
// Another routine can request a watcher for a resource by calling WatcherForResource.
// All watchers will be notified when the resource has successfully been created.
type ResourceStore struct {
	resources map[string]*Resource
	timeout   time.Duration
	closeChan chan struct{}
	closed    bool
	sync.Mutex
}

// Resource contains the actual resource itself (which must implement the IdentifiableCreatable interface),
// as well as stores function pointers that pertain to how that resource should be cleaned up,
// and keeps track of other requests that are watching for the successful creation of this resource.
type Resource struct {
	resource IdentifiableCreatable
	cleaner  *ResourceCleaner
	watchers []chan struct{}
	stale    bool
	name     string
}

// wasPut checks that a resource has been fully defined yet.
// This is defined as a resource that only has watchers, but no associated resource.
func (r *Resource) wasPut() bool {
	return r != nil && r.resource != nil
}

// IdentifiableCreatable are the qualities needed by the caller of the resource.
// Once a resource is retrieved, SetCreated() will be called, indicating to the server
// that resource is ready to be listed and operated upon, and ID() will be used to identify the
// newly created resource to the server.
type IdentifiableCreatable interface {
	ID() string
	SetCreated()
}

// New creates a new ResourceStore, with a default timeout, and starts the cleanup function
func New() *ResourceStore {
	return NewWithTimeout(sleepTimeBeforeCleanup)
}

// NewWithTimeout is used for testing purposes. It allows the caller to set the timeout, allowing for faster tests.
// Most callers should use New instead.
func NewWithTimeout(timeout time.Duration) *ResourceStore {
	rc := &ResourceStore{
		resources: make(map[string]*Resource),
		closeChan: make(chan struct{}, 1),
		timeout:   timeout,
	}
	go rc.cleanupStaleResources()
	return rc
}

func (rc *ResourceStore) Close() {
	rc.Lock()
	defer rc.Unlock()
	if rc.closed {
		return
	}
	close(rc.closeChan)
	rc.closed = true
}

// cleanupStaleResources is responsible for cleaning up resources that haven't been gotten
// from the store.
// It runs on a loop, sleeping `sleepTimeBeforeCleanup` between each loop.
// A resource will first be marked as stale before being cleaned up.
// This means a resource will stay in the store between `sleepTimeBeforeCleanup` and `2*sleepTimeBeforeCleanup`.
// When a resource is cleaned up, it's removed from the store and the cleanup funcs in its cleaner are called.
func (rc *ResourceStore) cleanupStaleResources() {
	for {
		select {
		case <-rc.closeChan:
			return
		case <-time.After(rc.timeout):
		}
		resourcesToReap := []*Resource{}
		rc.Lock()
		for name, r := range rc.resources {
			// this resource shouldn't be marked as stale if it
			// hasn't yet been added to the store.
			// This can happen if a creation is in progress, and a watcher is added
			// before the creation completes.
			// If this resource isn't skipped from being marked as stale,
			// we risk segfaulting in the Cleanup() step.
			if !r.wasPut() {
				continue
			}
			if r.stale {
				resourcesToReap = append(resourcesToReap, r)
				delete(rc.resources, name)
			}
			r.stale = true
		}
		// no need to hold the lock when running the cleanup functions
		rc.Unlock()

		for _, r := range resourcesToReap {
			logrus.Infof("cleaning up stale resource %s", r.name)
			r.cleaner.Cleanup()
		}
	}
}

// Get attempts to look up a resource by its name.
// If it's found, it's removed from the store, and it is set as created.
// Get returns an empty ID if the resource is not found,
// and returns the value of the Resource's ID() method if it is.
func (rc *ResourceStore) Get(name string) string {
	rc.Lock()
	defer rc.Unlock()

	r, ok := rc.resources[name]
	if !ok {
		return ""
	}
	// It is possible there are existing watchers,
	// but no resource created yet
	if !r.wasPut() {
		return ""
	}
	delete(rc.resources, name)
	r.resource.SetCreated()
	return r.resource.ID()
}

// Put takes a unique resource name (retrieved from the client request, not generated by the server),
// a newly created resource, and functions to clean up that newly created resource.
// It adds the Resource to the ResourceStore. It expects name to be unique, and
// returns an error if a duplicate name is detected.
func (rc *ResourceStore) Put(name string, resource IdentifiableCreatable, cleaner *ResourceCleaner) error {
	rc.Lock()
	defer rc.Unlock()

	r, ok := rc.resources[name]
	// if we don't already have a resource, create it
	if !ok {
		r = &Resource{}
		rc.resources[name] = r
	}
	// make sure the resource hasn't already been added to the store
	if ok && r.wasPut() {
		return errors.Errorf("failed to add entry %s to ResourceStore; entry already exists", name)
	}

	r.resource = resource
	r.cleaner = cleaner
	r.name = name

	// now the resource is created, notify the watchers
	for _, w := range r.watchers {
		w <- struct{}{}
	}
	return nil
}

// WatcherForResource looks up a Resource by name, and gives it a watcher.
// If no entry exists for that resource, a placeholder is created and a watcher is given to that
// placeholder resource.
// A watcher can be used for concurrent processes to wait for the resource to be created.
// This is useful for situations where clients retry requests quickly after they "fail" because
// they've taken too long. Adding a watcher allows the server to slow down the client, but still
// return the resource in a timely manner once it's actually created.
func (rc *ResourceStore) WatcherForResource(name string) chan struct{} {
	rc.Lock()
	defer rc.Unlock()
	watcher := make(chan struct{}, 1)
	r, ok := rc.resources[name]
	if !ok {
		rc.resources[name] = &Resource{
			watchers: []chan struct{}{watcher},
			name:     name,
		}
		return watcher
	}
	r.watchers = append(r.watchers, watcher)
	return watcher
}

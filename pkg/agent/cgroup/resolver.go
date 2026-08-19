package cgroup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Resolver maps pod UIDs onto cgroup paths.
//
// The kubelet's cgroup layout depends on the cgroup driver, the configured
// cgroup root and the pod's native QoS class, which produces at least four
// plausible paths for any given pod:
//
//	cgroupfs: /kubepods/pod<uid>                       (guaranteed)
//	cgroupfs: /kubepods/burstable/pod<uid>
//	systemd:  /kubelet.slice/kubelet-kubepods.slice/kubelet-kubepods-pod<uid>.slice
//	systemd:  /kubelet.slice/kubelet-kubepods.slice/kubelet-kubepods-burstable.slice/...
//
// Rather than reimplementing the kubelet's naming rules and re-breaking on
// every release, the resolver searches the tree once per pod and caches the
// hit. A bounded-depth walk over a few hundred directories costs well under a
// millisecond and is immune to layout changes.
type Resolver struct {
	fs       *FS
	maxDepth int

	mu    sync.RWMutex
	cache map[string]string // pod UID -> cgroup-relative path

	kubepodsOnce sync.Once
	kubepodsPath string
}

// NewResolver builds a resolver over the given hierarchy.
func NewResolver(fs *FS) *Resolver {
	return &Resolver{
		fs:       fs,
		maxDepth: 6,
		cache:    make(map[string]string),
	}
}

// normalizeUID renders a pod UID the way the systemd cgroup driver does, which
// replaces dashes with underscores because dashes are hierarchy separators in
// slice names.
func normalizeUID(uid string) string {
	return strings.ReplaceAll(uid, "-", "_")
}

// PodPath returns the cgroup-relative path of a pod's cgroup.
func (r *Resolver) PodPath(uid string) (string, error) {
	r.mu.RLock()
	if p, ok := r.cache[uid]; ok {
		r.mu.RUnlock()
		// Re-validate: pods die and their cgroups vanish, and a stale hit would
		// silently report the last reading forever.
		if r.fs.Exists(p) {
			return p, nil
		}
		r.mu.Lock()
		delete(r.cache, uid)
		r.mu.Unlock()
	} else {
		r.mu.RUnlock()
	}

	needles := []string{"pod" + uid, "pod" + normalizeUID(uid)}
	found, err := r.search(func(name string) bool {
		for _, n := range needles {
			if strings.Contains(name, n) {
				return true
			}
		}
		return false
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("no cgroup found for pod %s under %s", uid, r.fs.root)
	}

	r.mu.Lock()
	r.cache[uid] = found
	r.mu.Unlock()
	return found, nil
}

// ContainerPaths returns the cgroup paths of the containers inside a pod. With
// the systemd driver these are the *.scope children; with cgroupfs they are
// directories named after the container id. Either way they are simply the
// pod cgroup's children, so no driver-specific parsing is needed.
func (r *Resolver) ContainerPaths(uid string) ([]string, error) {
	podPath, err := r.PodPath(uid)
	if err != nil {
		return nil, err
	}
	children, err := r.fs.ListChildren(podPath)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(children))
	for _, c := range children {
		out = append(out, filepath.Join(podPath, c))
	}
	return out, nil
}

// KubepodsPath returns the cgroup that contains every pod on the node. Summing
// usage here is both cheaper and more accurate than adding up individual pods,
// because it captures the pod-infra containers too.
func (r *Resolver) KubepodsPath() string {
	r.kubepodsOnce.Do(func() {
		found, err := r.search(func(name string) bool {
			return name == "kubepods" ||
				name == "kubepods.slice" ||
				name == "kubelet-kubepods.slice"
		})
		if err == nil {
			r.kubepodsPath = found
		}
	})
	return r.kubepodsPath
}

// search walks the hierarchy breadth-first up to maxDepth, returning the first
// directory whose base name satisfies match. Breadth-first matters: the
// shallowest match is the pod's own cgroup rather than one of its containers.
func (r *Resolver) search(match func(name string) bool) (string, error) {
	type item struct {
		rel   string
		depth int
	}
	queue := []item{{rel: "", depth: 0}}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		entries, err := os.ReadDir(filepath.Join(r.fs.root, cur.rel))
		if err != nil {
			// Unreadable subtrees are normal in a container; keep walking.
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			rel := filepath.Join(cur.rel, e.Name())
			if match(e.Name()) {
				return rel, nil
			}
			if cur.depth+1 < r.maxDepth {
				queue = append(queue, item{rel: rel, depth: cur.depth + 1})
			}
		}
	}
	return "", nil
}

// Forget drops a cached entry, used when a pod is observed to have terminated.
func (r *Resolver) Forget(uid string) {
	r.mu.Lock()
	delete(r.cache, uid)
	r.mu.Unlock()
}

// CachedCount reports how many pod paths are memoised, exported as a metric so
// a leak in Forget shows up rather than growing silently.
func (r *Resolver) CachedCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.cache)
}

// Package podwatch is a minimal client-go informer wrapper. One Watcher
// emits add/update/delete events for the pods of a single Target (namespace
// + label selector). Consumers correlate the (target, pod) pair to start and
// stop per-pod work (gRPC stream subscriptions in this repo).
package podwatch

import (
	"context"
	"fmt"
	"sync"

	"github.com/coding-workspace/simple-mitigation-1/pkg/targets"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// EventType classifies a pod change.
type EventType int

const (
	EventAdd EventType = iota
	EventUpdate
	EventDelete
)

func (e EventType) String() string {
	switch e {
	case EventAdd:
		return "add"
	case EventUpdate:
		return "update"
	case EventDelete:
		return "delete"
	}
	return "?"
}

// Event is a single pod change. Target is retained so a consumer fanning in
// multiple watchers can attribute the event without an extra lookup.
type Event struct {
	Type     EventType
	Target   *targets.Target
	PodName  string
	PodIP    string
	NodeName string
	Ready    bool
}

// Watcher streams pod events for a single target.
type Watcher struct {
	target *targets.Target
	client kubernetes.Interface
	selStr string
	// nodeFieldSel narrows the informer to pods on a single node when the
	// watcher was constructed via NewLocalNodeWatcher. Empty means cluster-wide.
	nodeFieldSel string

	out      chan Event
	stopCh   chan struct{}
	stopOnce sync.Once
}

// NewWatcher constructs (but does not start) a Watcher for target.
func NewWatcher(client kubernetes.Interface, target *targets.Target) (*Watcher, error) {
	if target == nil {
		return nil, fmt.Errorf("podwatch: nil target")
	}
	if len(target.Selector) == 0 {
		return nil, fmt.Errorf("podwatch: empty selector for target %q", target.Name)
	}
	sel := labels.SelectorFromSet(labels.Set(target.Selector)).String()
	return &Watcher{
		target: target,
		client: client,
		selStr: sel,
		out:    make(chan Event, 64),
		stopCh: make(chan struct{}),
	}, nil
}

// NewLocalNodeWatcher is like NewWatcher but restricts the informer to pods
// whose spec.nodeName matches nodeName. Used by the per-node DaemonSet
// controller so each instance only ever sees its own victim pods. nodeName
// must be non-empty.
func NewLocalNodeWatcher(client kubernetes.Interface, target *targets.Target, nodeName string) (*Watcher, error) {
	if nodeName == "" {
		return nil, fmt.Errorf("podwatch: nodeName is required for local-node watcher")
	}
	w, err := NewWatcher(client, target)
	if err != nil {
		return nil, err
	}
	w.nodeFieldSel = "spec.nodeName=" + nodeName
	return w, nil
}

// Run starts the informer and blocks until ctx is cancelled or Stop is called.
// Returns nil on clean stop.
func (w *Watcher) Run(ctx context.Context) error {
	factory := informers.NewSharedInformerFactoryWithOptions(
		w.client,
		0, // no resync
		informers.WithNamespace(w.target.Namespace),
		informers.WithTweakListOptions(func(lo *metav1.ListOptions) {
			lo.LabelSelector = w.selStr
			if w.nodeFieldSel != "" {
				lo.FieldSelector = w.nodeFieldSel
			}
		}),
	)
	informer := factory.Core().V1().Pods().Informer()
	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { w.dispatch(EventAdd, obj) },
		UpdateFunc: func(_, obj interface{}) { w.dispatch(EventUpdate, obj) },
		DeleteFunc: func(obj interface{}) {
			if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = d.Obj
			}
			w.dispatch(EventDelete, obj)
		},
	}); err != nil {
		return fmt.Errorf("podwatch[%s]: add event handler: %w", w.target.Name, err)
	}

	factory.Start(w.stopCh)
	for typ, ok := range factory.WaitForCacheSync(w.stopCh) {
		if !ok {
			return fmt.Errorf("podwatch[%s]: cache sync failed for %v", w.target.Name, typ)
		}
	}

	select {
	case <-ctx.Done():
	case <-w.stopCh:
	}
	return nil
}

func (w *Watcher) dispatch(et EventType, obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	ev := Event{
		Type:     et,
		Target:   w.target,
		PodName:  pod.Name,
		PodIP:    pod.Status.PodIP,
		NodeName: pod.Spec.NodeName,
		Ready:    isPodReady(pod),
	}
	select {
	case w.out <- ev:
	case <-w.stopCh:
	}
}

func isPodReady(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// Events returns the channel of pod events. Buffer is 64; if the consumer
// falls behind, dispatch blocks until either the buffer drains or Stop is
// called.
func (w *Watcher) Events() <-chan Event { return w.out }

// Stop terminates the watcher. Safe to call multiple times.
func (w *Watcher) Stop() {
	w.stopOnce.Do(func() { close(w.stopCh) })
}

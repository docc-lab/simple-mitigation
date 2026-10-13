package hc

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

// stubClient is the slice of kubernetes.Interface the hc actuator uses:
// Pods List (label selector honoured; the spec.nodeName field selector is
// ignored, every stub pod is on the node), Get, Create, and annotation
// merge-patches. It avoids client-go's fake package, whose json-patch
// dependency is not in this module's go.sum. Unimplemented methods panic
// through the nil embedded interfaces.
type stubClient struct {
	kubernetes.Interface
	mu   sync.Mutex
	pods map[string]*corev1.Pod // ns/name
}

func newStubClient() *stubClient { return &stubClient{pods: map[string]*corev1.Pod{}} }

func (c *stubClient) CoreV1() typedcorev1.CoreV1Interface { return stubCore{c: c} }

type stubCore struct {
	typedcorev1.CoreV1Interface
	c *stubClient
}

func (s stubCore) Pods(ns string) typedcorev1.PodInterface { return stubPods{c: s.c, ns: ns} }

type stubPods struct {
	typedcorev1.PodInterface
	c  *stubClient
	ns string
}

func (p stubPods) Create(_ context.Context, pod *corev1.Pod, _ metav1.CreateOptions) (*corev1.Pod, error) {
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	cp := pod.DeepCopy()
	cp.Namespace = p.ns
	p.c.pods[p.ns+"/"+pod.Name] = cp
	return cp.DeepCopy(), nil
}

func (p stubPods) Get(_ context.Context, name string, _ metav1.GetOptions) (*corev1.Pod, error) {
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	pod, ok := p.c.pods[p.ns+"/"+name]
	if !ok {
		return nil, apierrors.NewNotFound(corev1.Resource("pods"), name)
	}
	return pod.DeepCopy(), nil
}

func (p stubPods) List(_ context.Context, opts metav1.ListOptions) (*corev1.PodList, error) {
	sel, err := labels.Parse(opts.LabelSelector)
	if err != nil {
		return nil, err
	}
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	keys := make([]string, 0, len(p.c.pods))
	for k := range p.c.pods {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := &corev1.PodList{}
	for _, k := range keys {
		pod := p.c.pods[k]
		if p.ns != metav1.NamespaceAll && pod.Namespace != p.ns {
			continue
		}
		if !sel.Matches(labels.Set(pod.Labels)) {
			continue
		}
		out.Items = append(out.Items, *pod.DeepCopy())
	}
	return out, nil
}

func (p stubPods) Patch(_ context.Context, name string, pt types.PatchType, data []byte, _ metav1.PatchOptions, _ ...string) (*corev1.Pod, error) {
	if pt != types.MergePatchType {
		return nil, fmt.Errorf("stub: only merge patch supported, got %s", pt)
	}
	var body struct {
		Metadata struct {
			Annotations map[string]*string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, err
	}
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	pod, ok := p.c.pods[p.ns+"/"+name]
	if !ok {
		return nil, apierrors.NewNotFound(corev1.Resource("pods"), name)
	}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	for k, v := range body.Metadata.Annotations {
		if v == nil {
			delete(pod.Annotations, k)
		} else {
			pod.Annotations[k] = *v
		}
	}
	return pod.DeepCopy(), nil
}

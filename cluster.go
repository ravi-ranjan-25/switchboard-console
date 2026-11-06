package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// KStatus is the read-only view of a Flux Kustomization.
type KStatus struct {
	Ready      string    `json:"ready"` // True | False | Unknown | ""
	Reason     string    `json:"reason"`
	Message    string    `json:"message"`
	Revision   string    `json:"revision"`
	Attempted  string    `json:"attempted"`
	LastChange time.Time `json:"last_change"`
	Suspended  bool      `json:"suspended"`
}

// Pill collapses a status into the label shown in the UI.
func (s KStatus) Pill() string {
	switch {
	case s.Suspended:
		return "Suspended"
	case s.Ready == "True":
		return "Ready"
	case s.Ready == "False":
		return "Failed"
	case s.Ready == "Unknown":
		return "Reconciling"
	}
	return "Unknown"
}

// PodInfo is the read-only view of a pod.
type PodInfo struct {
	Name       string    `json:"name"`
	Phase      string    `json:"phase"`
	Ready      string    `json:"ready"` // "1/1"
	Restarts   int32     `json:"restarts"`
	Reason     string    `json:"reason,omitempty"` // e.g. ImagePullBackOff, CrashLoopBackOff
	Image      string    `json:"image"`
	Started    time.Time `json:"started"`
	Containers []string  `json:"containers"`
}

// ImageStatus comes from the Flux ImageRepository/ImagePolicy status.
type ImageStatus struct {
	Image     string    `json:"image"`
	Tags      []string  `json:"tags"` // newest first, a sample (Flux keeps the latest ten)
	TagCount  int64     `json:"tag_count"`
	LastScan  time.Time `json:"last_scan"`
	PolicyTag string    `json:"policy_tag"` // what the ImagePolicy would deploy now
	Error     string    `json:"error,omitempty"`
}

// SecretStatus is an ExternalSecret's Ready condition. Never any values.
type SecretStatus struct {
	Ready   string `json:"ready"` // True | False | Unknown
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// Cluster is deliberately read-only except RequestReconcile.
type Cluster interface {
	Statuses(ctx context.Context) (map[string]KStatus, error)
	// RequestReconcile annotates the GitRepository: the ONLY cluster write.
	RequestReconcile(ctx context.Context) error
	Pods(ctx context.Context, ns, app string) ([]PodInfo, error)
	Logs(ctx context.Context, ns, pod, container string, tail int) (string, error)
	ImageStatus(ctx context.Context, name string) (*ImageStatus, error)
	ExternalSecrets(ctx context.Context, ns string) (map[string]SecretStatus, error)
}

type K8sCluster struct {
	dyn     dynamic.Interface
	kube    kubernetes.Interface
	ns      string
	gitRepo string
}

var (
	gvrKustomization = schema.GroupVersionResource{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"}
	gvrGitRepository = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "gitrepositories"}
	gvrImageRepo     = schema.GroupVersionResource{Group: "image.toolkit.fluxcd.io", Version: "v1", Resource: "imagerepositories"}
	gvrImagePolicy   = schema.GroupVersionResource{Group: "image.toolkit.fluxcd.io", Version: "v1", Resource: "imagepolicies"}
	gvrExternalSec   = schema.GroupVersionResource{Group: "external-secrets.io", Version: "v1", Resource: "externalsecrets"}
)

func NewK8sCluster(cfg *Config) (*K8sCluster, error) {
	rc, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	d, err := dynamic.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	kc, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	return newK8sClusterWith(d, kc, cfg.FluxNamespace, cfg.GitRepoName), nil
}

func newK8sClusterWith(d dynamic.Interface, kc kubernetes.Interface, ns, gitRepo string) *K8sCluster {
	return &K8sCluster{dyn: d, kube: kc, ns: ns, gitRepo: gitRepo}
}

func (k *K8sCluster) Statuses(ctx context.Context) (map[string]KStatus, error) {
	l, err := k.dyn.Resource(gvrKustomization).Namespace(k.ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := map[string]KStatus{}
	for i := range l.Items {
		out[l.Items[i].GetName()] = statusFrom(&l.Items[i])
	}
	return out, nil
}

func statusFrom(u *unstructured.Unstructured) KStatus {
	var s KStatus
	s.Suspended, _, _ = unstructured.NestedBool(u.Object, "spec", "suspend")
	s.Revision, _, _ = unstructured.NestedString(u.Object, "status", "lastAppliedRevision")
	s.Attempted, _, _ = unstructured.NestedString(u.Object, "status", "lastAttemptedRevision")
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != "Ready" {
			continue
		}
		s.Ready, _ = m["status"].(string)
		s.Reason, _ = m["reason"].(string)
		s.Message, _ = m["message"].(string)
		if ts, _ := m["lastTransitionTime"].(string); ts != "" {
			s.LastChange, _ = time.Parse(time.RFC3339, ts)
		}
	}
	return s
}

func (k *K8sCluster) RequestReconcile(ctx context.Context) error {
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{
		"reconcile.fluxcd.io/requestedAt": time.Now().UTC().Format(time.RFC3339Nano),
	}}})
	_, err := k.dyn.Resource(gvrGitRepository).Namespace(k.ns).Patch(ctx, k.gitRepo, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("annotate gitrepository: %w", err)
	}
	return nil
}

// revisionHasCommit reports whether a Flux revision ("main@sha1:<sha>" or
// "main/<sha>") points at commit sha.
func revisionHasCommit(rev, sha string) bool {
	return sha != "" && strings.Contains(rev, sha)
}

func (k *K8sCluster) Pods(ctx context.Context, ns, app string) ([]PodInfo, error) {
	l, err := k.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "citadel.dev/app=" + app})
	if err != nil {
		return nil, err
	}
	var out []PodInfo
	for i := range l.Items {
		out = append(out, podInfo(&l.Items[i]))
	}
	return out, nil
}

func podInfo(p *corev1.Pod) PodInfo {
	pi := PodInfo{Name: p.Name, Phase: string(p.Status.Phase)}
	if p.Status.StartTime != nil {
		pi.Started = p.Status.StartTime.Time
	}
	ready := 0
	for _, c := range p.Spec.Containers {
		pi.Containers = append(pi.Containers, c.Name)
	}
	if len(p.Spec.Containers) > 0 {
		pi.Image = p.Spec.Containers[0].Image
	}
	all := append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
	for _, cs := range all {
		pi.Restarts += cs.RestartCount
		if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "PodInitializing" && pi.Reason == "" {
			pi.Reason = w.Reason
		}
		if t := cs.State.Terminated; t != nil && t.ExitCode != 0 && pi.Reason == "" {
			pi.Reason = t.Reason
		}
	}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Ready {
			ready++
		}
	}
	pi.Ready = fmt.Sprintf("%d/%d", ready, len(p.Spec.Containers))
	if p.DeletionTimestamp != nil {
		pi.Phase = "Terminating"
	}
	return pi
}

func (k *K8sCluster) Logs(ctx context.Context, ns, pod, container string, tail int) (string, error) {
	t := int64(tail)
	b, err := k.kube.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{Container: container, TailLines: &t, LimitBytes: ptr(int64(64 << 10))}).DoRaw(ctx)
	return string(b), err
}

func ptr[T any](v T) *T { return &v }

func (k *K8sCluster) ImageStatus(ctx context.Context, name string) (*ImageStatus, error) {
	ir, err := k.dyn.Resource(gvrImageRepo).Namespace(k.ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	st := &ImageStatus{}
	st.Image, _, _ = unstructured.NestedString(ir.Object, "spec", "image")
	st.Tags, _, _ = unstructured.NestedStringSlice(ir.Object, "status", "lastScanResult", "latestTags")
	st.TagCount, _, _ = unstructured.NestedInt64(ir.Object, "status", "lastScanResult", "tagCount")
	if ts, _, _ := unstructured.NestedString(ir.Object, "status", "lastScanResult", "scanTime"); ts != "" {
		st.LastScan, _ = time.Parse(time.RFC3339, ts)
	}
	for _, c := range readyConds(ir) {
		if c.status == "False" {
			st.Error = c.message
		}
	}
	if ip, err := k.dyn.Resource(gvrImagePolicy).Namespace(k.ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
		ref, _, _ := unstructured.NestedString(ip.Object, "status", "latestRef", "tag")
		st.PolicyTag = ref
	}
	return st, nil
}

type cond struct{ status, reason, message string }

func readyConds(u *unstructured.Unstructured) []cond {
	var out []cond
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		if m, ok := c.(map[string]any); ok && m["type"] == "Ready" {
			s, _ := m["status"].(string)
			r, _ := m["reason"].(string)
			msg, _ := m["message"].(string)
			out = append(out, cond{s, r, msg})
		}
	}
	return out
}

func (k *K8sCluster) ExternalSecrets(ctx context.Context, ns string) (map[string]SecretStatus, error) {
	l, err := k.dyn.Resource(gvrExternalSec).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := map[string]SecretStatus{}
	for i := range l.Items {
		ss := SecretStatus{Ready: "Unknown"}
		for _, c := range readyConds(&l.Items[i]) {
			ss = SecretStatus{c.status, c.reason, c.message}
		}
		out[l.Items[i].GetName()] = ss
	}
	return out, nil
}

// noCluster is for local development without a cluster.
type noCluster struct{}

func (noCluster) Statuses(context.Context) (map[string]KStatus, error) {
	return map[string]KStatus{}, nil
}
func (noCluster) RequestReconcile(context.Context) error { return nil }
func (noCluster) Pods(context.Context, string, string) ([]PodInfo, error) {
	return nil, nil
}
func (noCluster) Logs(context.Context, string, string, string, int) (string, error) {
	return "", errors.New("no cluster")
}
func (noCluster) ImageStatus(context.Context, string) (*ImageStatus, error) {
	return nil, errors.New("no cluster")
}
func (noCluster) ExternalSecrets(context.Context, string) (map[string]SecretStatus, error) {
	return nil, nil
}

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Pool manages a set of warm demo pods for per-visitor assignment.
type Pool struct {
	cfg    Config
	client kubernetes.Interface

	mu       sync.Mutex
	warm     []podInfo // ready pods waiting for visitors
	assigned map[string]podInfo // token -> assigned pod
}

type podInfo struct {
	Name      string
	IP        string
	Token     string
	CreatedAt time.Time
}

func NewPool(cfg Config) (*Pool, error) {
	client, err := newK8sClient()
	if err != nil {
		return nil, fmt.Errorf("k8s client: %w", err)
	}
	return &Pool{
		cfg:      cfg,
		client:   client,
		assigned: make(map[string]podInfo),
	}, nil
}

func newK8sClient() (kubernetes.Interface, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		config, err = clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
		if err != nil {
			return nil, err
		}
	}
	return kubernetes.NewForConfig(config)
}

// Start begins the pool management loops.
func (p *Pool) Start(ctx context.Context) {
	// Initial fill
	p.replenish(ctx)

	go p.replenishLoop(ctx)
	go p.reapLoop(ctx)
}

// replenishLoop ensures the warm pool stays at the target size.
func (p *Pool) replenishLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.replenish(ctx)
		}
	}
}

// reapLoop cleans up expired assigned pods.
func (p *Pool) reapLoop(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.reapExpired(ctx)
		}
	}
}

func (p *Pool) replenish(ctx context.Context) {
	p.mu.Lock()
	need := p.cfg.PoolSize - len(p.warm)
	p.mu.Unlock()

	for i := 0; i < need; i++ {
		pod, err := p.createPod(ctx)
		if err != nil {
			log.Printf("create warm pod: %v", err)
			return
		}
		log.Printf("created warm pod %s", pod.Name)
	}
}

func (p *Pool) reapExpired(ctx context.Context) {
	p.mu.Lock()
	var expired []string
	for token, pod := range p.assigned {
		if time.Since(pod.CreatedAt) > time.Duration(p.cfg.PodMaxAge)*time.Second {
			expired = append(expired, token)
		}
	}
	for _, token := range expired {
		pod := p.assigned[token]
		delete(p.assigned, token)
		go p.deletePod(ctx, pod.Name)
	}
	p.mu.Unlock()
}

func (p *Pool) createPod(ctx context.Context) (*podInfo, error) {
	id := randomID(8)
	name := fmt.Sprintf("demo-%s", id)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: p.cfg.Namespace,
			Labels: map[string]string{
				"app":  "codewire-demo",
				"role": "demo-pod",
			},
		},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: boolPtr(false),
			EnableServiceLinks:           boolPtr(false),
			DNSPolicy:                    corev1.DNSNone,
			DNSConfig:                    &corev1.PodDNSConfig{},
			RestartPolicy:                corev1.RestartPolicyNever,
			ActiveDeadlineSeconds:        int64Ptr(int64(p.cfg.PodMaxAge)),
			Containers: []corev1.Container{
				{
					Name:  "demo",
					Image: p.cfg.DemoImage,
					Ports: []corev1.ContainerPort{
						{ContainerPort: 7681, Protocol: corev1.ProtocolTCP},
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:              resource.MustParse("200m"),
							corev1.ResourceMemory:           resource.MustParse("128Mi"),
							corev1.ResourceEphemeralStorage: resource.MustParse("100Mi"),
						},
					},
					SecurityContext: &corev1.SecurityContext{
						RunAsNonRoot:             boolPtr(true),
						ReadOnlyRootFilesystem:   boolPtr(true),
						AllowPrivilegeEscalation: boolPtr(false),
						Capabilities: &corev1.Capabilities{
							Drop: []corev1.Capability{"ALL"},
						},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "tmp", MountPath: "/tmp"},
						{Name: "codewire", MountPath: "/home/demo/.codewire"},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "tmp",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{
							SizeLimit: resourcePtr("100Mi"),
						},
					},
				},
				{
					Name: "codewire",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{
							SizeLimit: resourcePtr("100Mi"),
						},
					},
				},
			},
		},
	}

	created, err := p.client.CoreV1().Pods(p.cfg.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}

	info := podInfo{
		Name:      created.Name,
		CreatedAt: time.Now(),
	}

	// Wait for pod IP (poll briefly)
	for i := 0; i < 30; i++ {
		time.Sleep(1 * time.Second)
		got, err := p.client.CoreV1().Pods(p.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			continue
		}
		if got.Status.PodIP != "" && got.Status.Phase == corev1.PodRunning {
			info.IP = got.Status.PodIP
			break
		}
	}

	if info.IP == "" {
		// Pod didn't become ready — clean up
		p.deletePod(ctx, name)
		return nil, fmt.Errorf("pod %s did not get IP", name)
	}

	p.mu.Lock()
	p.warm = append(p.warm, info)
	p.mu.Unlock()

	return &info, nil
}

func (p *Pool) deletePod(ctx context.Context, name string) {
	err := p.client.CoreV1().Pods(p.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		log.Printf("delete pod %s: %v", name, err)
	} else {
		log.Printf("deleted pod %s", name)
	}
}

// Assign pops a warm pod and returns it with a token.
func (p *Pool) Assign() (*podInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.warm) == 0 {
		return nil, fmt.Errorf("no warm pods available")
	}

	pod := p.warm[0]
	p.warm = p.warm[1:]
	pod.Token = randomID(16)
	p.assigned[pod.Token] = pod
	return &pod, nil
}

// Lookup finds an assigned pod by token.
func (p *Pool) Lookup(token string) (*podInfo, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pod, ok := p.assigned[token]
	if !ok {
		return nil, false
	}
	return &pod, true
}

// HandleHealth returns pool status.
func (p *Pool) HandleHealth(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	warmCount := len(p.warm)
	assignedCount := len(p.assigned)
	p.mu.Unlock()

	status := "ok"
	if warmCount == 0 {
		status = "no_warm_pods"
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":   status,
		"warm":     warmCount,
		"assigned": assignedCount,
	})
}

// HandleSession assigns a warm pod to a visitor and returns the WebSocket URL.
func (p *Pool) HandleSession(w http.ResponseWriter, r *http.Request) {
	pod, err := p.Assign()
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]any{
			"error": "Demo is at capacity. Please try again in a minute.",
		})
		return
	}

	// Build the WebSocket URL for the browser to connect to the broker's proxy
	scheme := "wss"
	host := r.Host
	if r.TLS == nil && (r.Header.Get("X-Forwarded-Proto") != "https") {
		scheme = "ws"
	}
	wsURL := fmt.Sprintf("%s://%s/ws/%s?token=%s", scheme, host, pod.Name, pod.Token)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ws":      wsURL,
		"pod":     pod.Name,
		"expires": p.cfg.PodMaxAge,
	})

	log.Printf("assigned pod %s (IP %s) to visitor from %s", pod.Name, pod.IP, r.RemoteAddr)
}

func randomID(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func boolPtr(v bool) *bool      { return &v }
func int64Ptr(v int64) *int64    { return &v }

func resourcePtr(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

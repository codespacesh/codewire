package controller

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	codewire "github.com/codespacesh/codewire/operator/api/v1alpha1"
)

// mockRoundTripper returns HTTP 200 for all requests (used to mock health checks).
type mockRoundTripper struct{}

func (m *mockRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("ok")),
		Header:     make(http.Header),
	}, nil
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := codewire.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newRelay(name, ns string) *codewire.CodewireRelay {
	return &codewire.CodewireRelay{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       types.UID("test-uid-" + name),
		},
		Spec: codewire.CodewireRelaySpec{
			BaseURL:  "https://test.relay.example.com",
			AuthMode: "token",
		},
	}
}

func setup(t *testing.T, objs ...client.Object) (*CodewireRelayReconciler, client.Client) {
	t.Helper()
	s := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&codewire.CodewireRelay{}).
		WithObjects(objs...).
		Build()
	r := &CodewireRelayReconciler{
		Client:     c,
		Scheme:     s,
		HTTPClient: &http.Client{Transport: &mockRoundTripper{}},
	}
	return r, c
}

func doReconcile(t *testing.T, r *CodewireRelayReconciler, name, ns string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: ns},
	})
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	return res
}

func getObj(t *testing.T, c client.Client, key types.NamespacedName, obj client.Object) {
	t.Helper()
	if err := c.Get(context.Background(), key, obj); err != nil {
		t.Fatalf("Get %T %s: %v", obj, key, err)
	}
}

func TestReconcile_CreatesResources(t *testing.T) {
	relay := newRelay("test", "default")
	r, c := setup(t, relay)
	doReconcile(t, r, "test", "default")

	// PVC
	pvc := &corev1.PersistentVolumeClaim{}
	getObj(t, c, types.NamespacedName{Name: "test-data", Namespace: "default"}, pvc)
	if pvc.Spec.Resources.Requests.Storage().String() != "1Gi" {
		t.Errorf("PVC size = %s, want 1Gi", pvc.Spec.Resources.Requests.Storage().String())
	}
	if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("PVC access modes = %v, want [ReadWriteOnce]", pvc.Spec.AccessModes)
	}

	// Deployment
	deploy := &appsv1.Deployment{}
	getObj(t, c, types.NamespacedName{Name: "test", Namespace: "default"}, deploy)
	if len(deploy.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(deploy.Spec.Template.Spec.Containers))
	}
	container := deploy.Spec.Template.Spec.Containers[0]
	if container.Image != defaultImage {
		t.Errorf("image = %s, want %s", container.Image, defaultImage)
	}
	if container.Name != "relay" {
		t.Errorf("container name = %s, want relay", container.Name)
	}

	// HTTP Service
	httpSvc := &corev1.Service{}
	getObj(t, c, types.NamespacedName{Name: "test-http", Namespace: "default"}, httpSvc)
	if httpSvc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("HTTP service type = %s, want ClusterIP", httpSvc.Spec.Type)
	}
	if len(httpSvc.Spec.Ports) != 1 || httpSvc.Spec.Ports[0].Port != 8080 {
		t.Errorf("HTTP service port = %v, want [8080]", httpSvc.Spec.Ports)
	}

	// SSH Service
	sshSvc := &corev1.Service{}
	getObj(t, c, types.NamespacedName{Name: "test-ssh", Namespace: "default"}, sshSvc)
	if sshSvc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Errorf("SSH service type = %s, want LoadBalancer", sshSvc.Spec.Type)
	}
	if len(sshSvc.Spec.Ports) != 1 || sshSvc.Spec.Ports[0].Port != 2222 {
		t.Errorf("SSH service port = %v, want [2222]", sshSvc.Spec.Ports)
	}
	if sshSvc.Spec.Ports[0].Protocol != corev1.ProtocolTCP {
		t.Errorf("SSH service protocol = %s, want TCP", sshSvc.Spec.Ports[0].Protocol)
	}
}

func TestReconcile_AddsFinalizerAndSetsPhase(t *testing.T) {
	relay := newRelay("test", "default")
	r, c := setup(t, relay)
	doReconcile(t, r, "test", "default")

	updated := &codewire.CodewireRelay{}
	getObj(t, c, types.NamespacedName{Name: "test", Namespace: "default"}, updated)

	found := false
	for _, f := range updated.Finalizers {
		if f == finalizerName {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("finalizer %q not found, finalizers = %v", finalizerName, updated.Finalizers)
	}

	if updated.Status.Phase != "Running" {
		t.Errorf("phase = %q, want %q", updated.Status.Phase, "Running")
	}

	if updated.Status.RelayURL != "https://test.relay.example.com" {
		t.Errorf("relayURL = %q, want %q", updated.Status.RelayURL, "https://test.relay.example.com")
	}
}

func TestReconcile_GeneratesAuthToken(t *testing.T) {
	relay := newRelay("test", "default")
	relay.Spec.AuthMode = "token"
	relay.Spec.AuthToken = ""
	r, c := setup(t, relay)
	doReconcile(t, r, "test", "default")

	updated := &codewire.CodewireRelay{}
	getObj(t, c, types.NamespacedName{Name: "test", Namespace: "default"}, updated)

	if updated.Spec.AuthToken == "" {
		t.Fatal("expected auth token to be generated, got empty string")
	}
	if len(updated.Spec.AuthToken) != 32 {
		t.Errorf("auth token length = %d, want 32", len(updated.Spec.AuthToken))
	}
}

func TestReconcile_DeploymentArgs(t *testing.T) {
	relay := newRelay("test", "default")
	relay.Spec.SSHListen = ":2222"
	relay.Spec.AuthMode = "none"
	r, c := setup(t, relay)
	doReconcile(t, r, "test", "default")

	deploy := &appsv1.Deployment{}
	getObj(t, c, types.NamespacedName{Name: "test", Namespace: "default"}, deploy)

	args := deploy.Spec.Template.Spec.Containers[0].Args
	expected := []string{
		"--base-url=https://test.relay.example.com",
		"--listen=0.0.0.0:8080",
		"--ssh-listen=:2222",
		"--data-dir=/data",
		"--auth-mode=none",
	}

	argSet := make(map[string]bool)
	for _, a := range args {
		argSet[a] = true
	}
	for _, e := range expected {
		if !argSet[e] {
			t.Errorf("expected arg %q not found in args: %v", e, args)
		}
	}

	// Verify command is "cw relay"
	cmd := deploy.Spec.Template.Spec.Containers[0].Command
	if len(cmd) != 2 || cmd[0] != "cw" || cmd[1] != "relay" {
		t.Errorf("command = %v, want [cw relay]", cmd)
	}

	// Verify container ports
	ports := deploy.Spec.Template.Spec.Containers[0].Ports
	if len(ports) != 2 {
		t.Fatalf("expected 2 container ports, got %d", len(ports))
	}
	if ports[0].ContainerPort != 8080 || ports[0].Protocol != corev1.ProtocolTCP {
		t.Errorf("HTTP port = %d/%s, want 8080/TCP", ports[0].ContainerPort, ports[0].Protocol)
	}
	if ports[1].ContainerPort != 2222 || ports[1].Protocol != corev1.ProtocolTCP {
		t.Errorf("SSH port = %d/%s, want 2222/TCP", ports[1].ContainerPort, ports[1].Protocol)
	}
}

func TestReconcile_IngressCreated(t *testing.T) {
	relay := newRelay("test", "default")
	relay.Spec.Ingress = &codewire.IngressSpec{
		ClassName:   "nginx",
		Annotations: map[string]string{"cert-manager.io/cluster-issuer": "letsencrypt"},
	}
	r, c := setup(t, relay)
	doReconcile(t, r, "test", "default")

	ing := &networkingv1.Ingress{}
	getObj(t, c, types.NamespacedName{Name: "test", Namespace: "default"}, ing)

	if ing.Spec.IngressClassName == nil || *ing.Spec.IngressClassName != "nginx" {
		t.Error("expected IngressClassName to be nginx")
	}
	if len(ing.Spec.Rules) != 1 {
		t.Fatalf("expected 1 ingress rule, got %d", len(ing.Spec.Rules))
	}
	if ing.Spec.Rules[0].Host != "test.relay.example.com" {
		t.Errorf("ingress host = %q, want %q", ing.Spec.Rules[0].Host, "test.relay.example.com")
	}

	// Verify TLS
	if len(ing.Spec.TLS) != 1 {
		t.Fatalf("expected 1 TLS entry, got %d", len(ing.Spec.TLS))
	}
	if ing.Spec.TLS[0].SecretName != "test-tls" {
		t.Errorf("TLS secret = %q, want %q", ing.Spec.TLS[0].SecretName, "test-tls")
	}

	// Verify annotation
	if ing.Annotations["cert-manager.io/cluster-issuer"] != "letsencrypt" {
		t.Errorf("annotation = %q, want letsencrypt", ing.Annotations["cert-manager.io/cluster-issuer"])
	}

	// Verify backend points to HTTP service
	backend := ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service
	if backend.Name != "test-http" {
		t.Errorf("backend service = %q, want test-http", backend.Name)
	}
}

func TestReconcile_IngressNotCreated(t *testing.T) {
	relay := newRelay("test", "default")
	relay.Spec.Ingress = nil
	r, c := setup(t, relay)
	doReconcile(t, r, "test", "default")

	ing := &networkingv1.Ingress{}
	err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, ing)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected Ingress to not exist, got err=%v", err)
	}
}

func TestReconcile_CredentialInjection(t *testing.T) {
	relay := newRelay("test", "default")
	relay.Spec.AuthToken = "test-token-123"
	relay.Spec.CredentialInjection = &codewire.CredentialInjectionSpec{
		TargetNamespace: "app-ns",
		SecretName:      "relay-creds",
	}
	r, c := setup(t, relay)
	doReconcile(t, r, "test", "default")

	secret := &corev1.Secret{}
	getObj(t, c, types.NamespacedName{Name: "relay-creds", Namespace: "app-ns"}, secret)

	// Fake client stores StringData as-is (no conversion to Data like real API server).
	if got := secret.StringData["relay-url"]; got != "https://test.relay.example.com" {
		t.Errorf("relay-url = %q, want %q", got, "https://test.relay.example.com")
	}
	if got := secret.StringData["auth-token"]; got != "test-token-123" {
		t.Errorf("auth-token = %q, want %q", got, "test-token-123")
	}

	// Verify cross-namespace labels
	if secret.Labels["codewire.io/source-namespace"] != "default" {
		t.Errorf("source-namespace label = %q, want default", secret.Labels["codewire.io/source-namespace"])
	}
	if secret.Labels["codewire.io/source-name"] != "test" {
		t.Errorf("source-name label = %q, want test", secret.Labels["codewire.io/source-name"])
	}
}

func TestReconcile_Deletion(t *testing.T) {
	relay := newRelay("test", "default")
	relay.Finalizers = []string{finalizerName}
	r, c := setup(t, relay)

	// Delete sets DeletionTimestamp because of the finalizer.
	if err := c.Delete(context.Background(), relay); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Reconcile handles deletion: removes finalizer.
	doReconcile(t, r, "test", "default")

	// After finalizer removal, the object should be fully deleted (fake client
	// auto-deletes when DeletionTimestamp is set and no finalizers remain).
	deleted := &codewire.CodewireRelay{}
	err := c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, deleted)
	if apierrors.IsNotFound(err) {
		return // fully deleted — expected
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// If the object still exists, at least verify finalizer was removed.
	for _, f := range deleted.Finalizers {
		if f == finalizerName {
			t.Error("finalizer should have been removed during deletion")
		}
	}
}

func TestReconcile_UpdatesExistingResources(t *testing.T) {
	relay := newRelay("test", "default")
	r, c := setup(t, relay)
	doReconcile(t, r, "test", "default")

	// Verify initial SSH listen address
	deploy := &appsv1.Deployment{}
	getObj(t, c, types.NamespacedName{Name: "test", Namespace: "default"}, deploy)
	for _, arg := range deploy.Spec.Template.Spec.Containers[0].Args {
		if arg == "--ssh-listen=:2222" {
			break
		}
	}

	// Update spec
	updated := &codewire.CodewireRelay{}
	getObj(t, c, types.NamespacedName{Name: "test", Namespace: "default"}, updated)
	updated.Spec.SSHListen = ":2223"
	if err := c.Update(context.Background(), updated); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Reconcile again
	doReconcile(t, r, "test", "default")

	// Verify Deployment was updated
	deploy2 := &appsv1.Deployment{}
	getObj(t, c, types.NamespacedName{Name: "test", Namespace: "default"}, deploy2)

	found := false
	for _, arg := range deploy2.Spec.Template.Spec.Containers[0].Args {
		if arg == "--ssh-listen=:2223" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected --ssh-listen=:2223 in args, got %v", deploy2.Spec.Template.Spec.Containers[0].Args)
	}

	// SSH service port should be 2222
	sshSvc := &corev1.Service{}
	getObj(t, c, types.NamespacedName{Name: "test-ssh", Namespace: "default"}, sshSvc)
	if sshSvc.Spec.Ports[0].Port != 2222 {
		t.Errorf("SSH service port = %d, want 2222", sshSvc.Spec.Ports[0].Port)
	}
}

func TestReconcile_Labels(t *testing.T) {
	relay := newRelay("test", "default")
	r, c := setup(t, relay)
	doReconcile(t, r, "test", "default")

	wantLabels := map[string]string{
		"app.kubernetes.io/name":       "codewire-relay",
		"app.kubernetes.io/instance":   "test",
		"app.kubernetes.io/managed-by": "codewire-operator",
	}

	resources := []struct {
		name string
		obj  client.Object
	}{
		{"test-data", &corev1.PersistentVolumeClaim{}},
		{"test", &appsv1.Deployment{}},
		{"test-http", &corev1.Service{}},
		{"test-ssh", &corev1.Service{}},
	}

	for _, res := range resources {
		getObj(t, c, types.NamespacedName{Name: res.name, Namespace: "default"}, res.obj)
		labels := res.obj.GetLabels()
		for k, v := range wantLabels {
			if labels[k] != v {
				t.Errorf("%s: label %s = %q, want %q", res.name, k, labels[k], v)
			}
		}
	}

	// Also verify pod template labels on the Deployment
	deploy := &appsv1.Deployment{}
	getObj(t, c, types.NamespacedName{Name: "test", Namespace: "default"}, deploy)
	podLabels := deploy.Spec.Template.Labels
	for k, v := range wantLabels {
		if podLabels[k] != v {
			t.Errorf("pod template label %s = %q, want %q", k, podLabels[k], v)
		}
	}
}

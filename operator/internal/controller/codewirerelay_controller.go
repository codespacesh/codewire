package controller

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	codewire "github.com/codespacesh/codewire/operator/api/v1alpha1"
)

const (
	finalizerName = "codewire.io/finalizer"
	defaultImage  = "ghcr.io/codespacesh/codewire:latest"
)

// Condition types for CodewireRelay status.
const (
	ConditionReady               = "Ready"
	ConditionWireGuardReady      = "WireGuardReady"
	ConditionDNSConfigured       = "DNSConfigured"
	ConditionCredentialsInjected = "CredentialsInjected"
)

// CodewireRelayReconciler reconciles a CodewireRelay object.
type CodewireRelayReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	HTTPClient *http.Client
	Image      string // default relay image override
}

// +kubebuilder:rbac:groups=codewire.io,resources=codewirerelays,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=codewire.io,resources=codewirerelays/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=codewire.io,resources=codewirerelays/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete

// Reconcile handles the reconciliation loop for a CodewireRelay resource.
func (r *CodewireRelayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1. Fetch the CodewireRelay CR.
	var relay codewire.CodewireRelay
	if err := r.Get(ctx, req.NamespacedName, &relay); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("CodewireRelay resource not found, likely deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "unable to fetch CodewireRelay")
		return ctrl.Result{}, err
	}

	// 2. Handle deletion.
	if !relay.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&relay, finalizerName) {
			logger.Info("handling deletion for CodewireRelay")

			if relay.Spec.DNS != nil {
				if err := r.deleteDNSRecord(ctx, &relay); err != nil {
					logger.Error(err, "failed to delete DNS record during cleanup")
					// Continue with finalizer removal even if DNS cleanup fails,
					// to avoid blocking deletion indefinitely.
				}
			}

			controllerutil.RemoveFinalizer(&relay, finalizerName)
			if err := r.Update(ctx, &relay); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// 3. Add finalizer if not present.
	if !controllerutil.ContainsFinalizer(&relay, finalizerName) {
		controllerutil.AddFinalizer(&relay, finalizerName)
		if err := r.Update(ctx, &relay); err != nil {
			return ctrl.Result{}, err
		}
		// Re-fetch after update to avoid stale resource version.
		if err := r.Get(ctx, req.NamespacedName, &relay); err != nil {
			return ctrl.Result{}, err
		}
	}

	// 4. Set phase to Provisioning if empty or Pending.
	if relay.Status.Phase == "" || relay.Status.Phase == "Pending" {
		relay.Status.Phase = "Provisioning"
		relay.Status.RelayURL = relay.Spec.BaseURL
	}

	// 5. Generate auth token if needed.
	if relay.Spec.AuthMode == "token" && relay.Spec.AuthToken == "" {
		relay.Spec.AuthToken = generateToken(32)
		if err := r.Update(ctx, &relay); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Get(ctx, req.NamespacedName, &relay); err != nil {
			return ctrl.Result{}, err
		}
	}

	// 6. Run sub-reconcilers in order.
	reconcileErr := r.runSubReconcilers(ctx, &relay)

	// 7. Set phase based on outcome.
	if reconcileErr != nil {
		relay.Status.Phase = "Failed"
		logger.Error(reconcileErr, "reconciliation failed")
	} else {
		relay.Status.Phase = "Running"
	}

	// 8. Update status.
	if err := r.Status().Update(ctx, &relay); err != nil {
		logger.Error(err, "unable to update CodewireRelay status")
		return ctrl.Result{}, err
	}

	if reconcileErr != nil {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, reconcileErr
	}

	// 9. Requeue after 30 seconds for periodic reconciliation.
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// runSubReconcilers executes all sub-reconcilers in order. It returns the first
// error encountered, after setting the appropriate condition on the relay status.
func (r *CodewireRelayReconciler) runSubReconcilers(ctx context.Context, relay *codewire.CodewireRelay) error {
	logger := log.FromContext(ctx)

	// PVC
	if err := r.reconcilePVC(ctx, relay); err != nil {
		logger.Error(err, "failed to reconcile PVC")
		return fmt.Errorf("reconcile PVC: %w", err)
	}

	// Deployment
	if err := r.reconcileDeployment(ctx, relay); err != nil {
		logger.Error(err, "failed to reconcile Deployment")
		return fmt.Errorf("reconcile Deployment: %w", err)
	}

	// HTTP Service
	if err := r.reconcileHTTPService(ctx, relay); err != nil {
		logger.Error(err, "failed to reconcile HTTP Service")
		return fmt.Errorf("reconcile HTTP Service: %w", err)
	}

	// WireGuard Service
	if err := r.reconcileWireGuardService(ctx, relay); err != nil {
		logger.Error(err, "failed to reconcile WireGuard Service")
		return fmt.Errorf("reconcile WireGuard Service: %w", err)
	}

	// Ingress (optional)
	if relay.Spec.Ingress != nil {
		if err := r.reconcileIngress(ctx, relay); err != nil {
			logger.Error(err, "failed to reconcile Ingress")
			return fmt.Errorf("reconcile Ingress: %w", err)
		}
	}

	// WireGuard Endpoint (watch LoadBalancer)
	if err := r.reconcileWireGuardEndpoint(ctx, relay); err != nil {
		logger.Error(err, "failed to reconcile WireGuard endpoint")
		return fmt.Errorf("reconcile WireGuard endpoint: %w", err)
	}

	// DNS (optional)
	if relay.Spec.DNS != nil {
		if err := r.reconcileDNS(ctx, relay); err != nil {
			logger.Error(err, "failed to reconcile DNS")
			return fmt.Errorf("reconcile DNS: %w", err)
		}
	}

	// Health check
	if err := r.reconcileHealthCheck(ctx, relay); err != nil {
		logger.Error(err, "failed to reconcile health check")
		return fmt.Errorf("reconcile health check: %w", err)
	}

	// Credential injection (optional)
	if relay.Spec.CredentialInjection != nil {
		if err := r.reconcileCredentialInjection(ctx, relay); err != nil {
			logger.Error(err, "failed to reconcile credential injection")
			return fmt.Errorf("reconcile credential injection: %w", err)
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// Sub-reconcilers
// ---------------------------------------------------------------------------

// reconcilePVC ensures a PersistentVolumeClaim exists for the relay's data.
func (r *CodewireRelayReconciler) reconcilePVC(ctx context.Context, relay *codewire.CodewireRelay) error {
	pvcName := relay.Name + "-data"
	size := relay.Spec.Persistence.Size
	if size == "" {
		size = "1Gi"
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: relay.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pvc, func() error {
		if err := ctrl.SetControllerReference(relay, pvc, r.Scheme); err != nil {
			return err
		}

		pvc.Labels = labelsForRelay(relay)

		// PVC spec is immutable after creation, so only set on new objects.
		if pvc.CreationTimestamp.IsZero() {
			pvc.Spec = corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{
					corev1.ReadWriteOnce,
				},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse(size),
					},
				},
			}

			if relay.Spec.Persistence.StorageClass != "" {
				pvc.Spec.StorageClassName = &relay.Spec.Persistence.StorageClass
			}
		}

		return nil
	})

	return err
}

// reconcileDeployment ensures the relay Deployment exists and is up to date.
func (r *CodewireRelayReconciler) reconcileDeployment(ctx context.Context, relay *codewire.CodewireRelay) error {
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      relay.Name,
			Namespace: relay.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		if err := ctrl.SetControllerReference(relay, deploy, r.Scheme); err != nil {
			return err
		}

		labels := labelsForRelay(relay)
		deploy.Labels = labels

		replicas := int32(1)
		deploy.Spec.Replicas = &replicas

		deploy.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: labels,
		}

		// Build container args.
		wgPort := relay.Spec.WGPort
		if wgPort == 0 {
			wgPort = 41820
		}

		args := []string{
			fmt.Sprintf("--base-url=%s", relay.Spec.BaseURL),
			"--listen=0.0.0.0:8080",
			fmt.Sprintf("--wg-port=%d", wgPort),
			"--data-dir=/data",
			fmt.Sprintf("--auth-mode=%s", relay.Spec.AuthMode),
		}
		if relay.Spec.AuthMode == "token" && relay.Spec.AuthToken != "" {
			args = append(args, fmt.Sprintf("--auth-token=%s", relay.Spec.AuthToken))
		}

		// Build resource requirements.
		resources := corev1.ResourceRequirements{}
		if relay.Spec.Resources != nil {
			if relay.Spec.Resources.Requests.CPU != "" || relay.Spec.Resources.Requests.Memory != "" {
				resources.Requests = corev1.ResourceList{}
				if relay.Spec.Resources.Requests.CPU != "" {
					resources.Requests[corev1.ResourceCPU] = resource.MustParse(relay.Spec.Resources.Requests.CPU)
				}
				if relay.Spec.Resources.Requests.Memory != "" {
					resources.Requests[corev1.ResourceMemory] = resource.MustParse(relay.Spec.Resources.Requests.Memory)
				}
			}
			if relay.Spec.Resources.Limits.CPU != "" || relay.Spec.Resources.Limits.Memory != "" {
				resources.Limits = corev1.ResourceList{}
				if relay.Spec.Resources.Limits.CPU != "" {
					resources.Limits[corev1.ResourceCPU] = resource.MustParse(relay.Spec.Resources.Limits.CPU)
				}
				if relay.Spec.Resources.Limits.Memory != "" {
					resources.Limits[corev1.ResourceMemory] = resource.MustParse(relay.Spec.Resources.Limits.Memory)
				}
			}
		}

		deploy.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: labels,
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:    "relay",
						Image:   r.relayImage(relay),
						Command: []string{"cw", "relay"},
						Args:    args,
						Ports: []corev1.ContainerPort{
							{
								Name:          "http",
								ContainerPort: 8080,
								Protocol:      corev1.ProtocolTCP,
							},
							{
								Name:          "wireguard",
								ContainerPort: wgPort,
								Protocol:      corev1.ProtocolUDP,
							},
						},
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      "data",
								MountPath: "/data",
							},
						},
						Resources: resources,
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path:   "/healthz",
									Port:   intstr.FromInt32(8080),
									Scheme: corev1.URISchemeHTTP,
								},
							},
							InitialDelaySeconds: 10,
							PeriodSeconds:       30,
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path:   "/healthz",
									Port:   intstr.FromInt32(8080),
									Scheme: corev1.URISchemeHTTP,
								},
							},
							InitialDelaySeconds: 5,
							PeriodSeconds:       10,
						},
					},
				},
				Volumes: []corev1.Volume{
					{
						Name: "data",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: relay.Name + "-data",
							},
						},
					},
				},
			},
		}

		return nil
	})

	return err
}

// reconcileHTTPService ensures the ClusterIP service for the relay HTTP API exists.
func (r *CodewireRelayReconciler) reconcileHTTPService(ctx context.Context, relay *codewire.CodewireRelay) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      relay.Name + "-http",
			Namespace: relay.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		if err := ctrl.SetControllerReference(relay, svc, r.Scheme); err != nil {
			return err
		}

		svc.Labels = labelsForRelay(relay)
		svc.Spec.Type = corev1.ServiceTypeClusterIP
		svc.Spec.Selector = labelsForRelay(relay)
		svc.Spec.Ports = []corev1.ServicePort{
			{
				Name:       "http",
				Port:       8080,
				TargetPort: intstr.FromString("http"),
				Protocol:   corev1.ProtocolTCP,
			},
		}

		return nil
	})

	return err
}

// reconcileWireGuardService ensures the WireGuard service exists with the
// configured service type (default LoadBalancer).
func (r *CodewireRelayReconciler) reconcileWireGuardService(ctx context.Context, relay *codewire.CodewireRelay) error {
	wgPort := relay.Spec.WGPort
	if wgPort == 0 {
		wgPort = 41820
	}

	svcType := corev1.ServiceType(relay.Spec.WireGuard.Service.Type)
	if svcType == "" {
		svcType = corev1.ServiceTypeLoadBalancer
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      relay.Name + "-wireguard",
			Namespace: relay.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		if err := ctrl.SetControllerReference(relay, svc, r.Scheme); err != nil {
			return err
		}

		svc.Labels = labelsForRelay(relay)

		// Apply provider-specific annotations (e.g., for cloud LB configuration).
		if relay.Spec.WireGuard.Service.Annotations != nil {
			svc.Annotations = relay.Spec.WireGuard.Service.Annotations
		}

		svc.Spec.Type = svcType
		svc.Spec.Selector = labelsForRelay(relay)
		svc.Spec.Ports = []corev1.ServicePort{
			{
				Name:       "wireguard",
				Port:       wgPort,
				TargetPort: intstr.FromString("wireguard"),
				Protocol:   corev1.ProtocolUDP,
			},
		}

		return nil
	})

	return err
}

// reconcileWireGuardEndpoint watches the WireGuard LoadBalancer service for an
// external IP or hostname and updates the relay status accordingly.
func (r *CodewireRelayReconciler) reconcileWireGuardEndpoint(ctx context.Context, relay *codewire.CodewireRelay) error {
	wgPort := relay.Spec.WGPort
	if wgPort == 0 {
		wgPort = 41820
	}

	svc := &corev1.Service{}
	svcName := types.NamespacedName{
		Name:      relay.Name + "-wireguard",
		Namespace: relay.Namespace,
	}
	if err := r.Get(ctx, svcName, svc); err != nil {
		r.setCondition(relay, ConditionWireGuardReady, metav1.ConditionFalse,
			"ServiceNotFound", fmt.Sprintf("WireGuard service not found: %v", err))
		return err
	}

	// Extract external IP or hostname from LoadBalancer status.
	var endpoint string
	if len(svc.Status.LoadBalancer.Ingress) > 0 {
		ingress := svc.Status.LoadBalancer.Ingress[0]
		if ingress.IP != "" {
			endpoint = fmt.Sprintf("%s:%d", ingress.IP, wgPort)
		} else if ingress.Hostname != "" {
			endpoint = fmt.Sprintf("%s:%d", ingress.Hostname, wgPort)
		}
	}

	if endpoint == "" {
		r.setCondition(relay, ConditionWireGuardReady, metav1.ConditionFalse,
			"WaitingForLoadBalancer", "Waiting for LoadBalancer to assign an external IP")
		// Not an error: we'll pick it up on the next reconciliation.
		return nil
	}

	relay.Status.WireGuardEndpoint = endpoint
	r.setCondition(relay, ConditionWireGuardReady, metav1.ConditionTrue,
		"EndpointReady", fmt.Sprintf("WireGuard endpoint available at %s", endpoint))

	return nil
}

// reconcileIngress ensures the Ingress resource is created from the spec.
func (r *CodewireRelayReconciler) reconcileIngress(ctx context.Context, relay *codewire.CodewireRelay) error {
	// Parse hostname from the base URL.
	parsedURL, err := url.Parse(relay.Spec.BaseURL)
	if err != nil {
		return fmt.Errorf("invalid baseURL %q: %w", relay.Spec.BaseURL, err)
	}
	hostname := parsedURL.Hostname()

	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      relay.Name,
			Namespace: relay.Namespace,
		},
	}

	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, ing, func() error {
		if err := ctrl.SetControllerReference(relay, ing, r.Scheme); err != nil {
			return err
		}

		ing.Labels = labelsForRelay(relay)

		// Apply user-specified annotations.
		if relay.Spec.Ingress.Annotations != nil {
			ing.Annotations = relay.Spec.Ingress.Annotations
		}

		// Set IngressClassName if specified.
		if relay.Spec.Ingress.ClassName != "" {
			ing.Spec.IngressClassName = &relay.Spec.Ingress.ClassName
		}

		pathType := networkingv1.PathTypePrefix
		ing.Spec.Rules = []networkingv1.IngressRule{
			{
				Host: hostname,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{
							{
								Path:     "/",
								PathType: &pathType,
								Backend: networkingv1.IngressBackend{
									Service: &networkingv1.IngressServiceBackend{
										Name: relay.Name + "-http",
										Port: networkingv1.ServiceBackendPort{
											Name: "http",
										},
									},
								},
							},
						},
					},
				},
			},
		}

		ing.Spec.TLS = []networkingv1.IngressTLS{
			{
				Hosts:      []string{hostname},
				SecretName: relay.Name + "-tls",
			},
		}

		return nil
	})

	return err
}

// reconcileHealthCheck probes the relay's /healthz endpoint via the cluster-
// internal HTTP service to determine readiness.
func (r *CodewireRelayReconciler) reconcileHealthCheck(ctx context.Context, relay *codewire.CodewireRelay) error {
	httpClient := r.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}

	healthURL := fmt.Sprintf("http://%s-http.%s.svc.cluster.local:8080/healthz",
		relay.Name, relay.Namespace)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		r.setCondition(relay, ConditionReady, metav1.ConditionFalse,
			"HealthCheckError", fmt.Sprintf("failed to create health check request: %v", err))
		return err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		r.setCondition(relay, ConditionReady, metav1.ConditionFalse,
			"HealthCheckFailed", fmt.Sprintf("health check request failed: %v", err))
		// Health check failures are not fatal to reconciliation. The relay
		// may still be starting up. We report the condition but do not
		// return an error so that subsequent sub-reconcilers can still run.
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		r.setCondition(relay, ConditionReady, metav1.ConditionTrue,
			"Healthy", "Relay is healthy")
	} else {
		r.setCondition(relay, ConditionReady, metav1.ConditionFalse,
			"Unhealthy", fmt.Sprintf("health check returned status %d", resp.StatusCode))
	}

	return nil
}

// reconcileDNS ensures the DNS record for the relay is configured. Currently
// only Cloudflare is supported as a provider.
func (r *CodewireRelayReconciler) reconcileDNS(ctx context.Context, relay *codewire.CodewireRelay) error {
	if relay.Spec.DNS.Provider != "cloudflare" {
		r.setCondition(relay, ConditionDNSConfigured, metav1.ConditionFalse,
			"UnsupportedProvider", fmt.Sprintf("DNS provider %q is not supported", relay.Spec.DNS.Provider))
		return fmt.Errorf("unsupported DNS provider: %s", relay.Spec.DNS.Provider)
	}

	// Resolve the API token from the referenced Secret.
	apiToken, err := r.resolveSecretKeyRef(ctx, relay.Namespace, relay.Spec.DNS.APITokenSecretRef)
	if err != nil {
		r.setCondition(relay, ConditionDNSConfigured, metav1.ConditionFalse,
			"SecretError", fmt.Sprintf("failed to resolve DNS API token: %v", err))
		return fmt.Errorf("resolve DNS API token: %w", err)
	}

	// Parse hostname from the base URL.
	parsedURL, err := url.Parse(relay.Spec.BaseURL)
	if err != nil {
		return fmt.Errorf("invalid baseURL: %w", err)
	}
	hostname := parsedURL.Hostname()

	// Determine the target value for the DNS record.
	// Prefer the WireGuard endpoint's IP if available, otherwise fall back
	// to using the Ingress external address.
	target := ""
	if relay.Status.WireGuardEndpoint != "" {
		// Parse out just the IP/hostname (strip port).
		epURL, parseErr := url.Parse("//" + relay.Status.WireGuardEndpoint)
		if parseErr == nil {
			target = epURL.Hostname()
		}
	}

	if target == "" {
		r.setCondition(relay, ConditionDNSConfigured, metav1.ConditionFalse,
			"NoTarget", "No external IP available for DNS record")
		return nil // Not an error; will resolve on next loop.
	}

	// Create/update DNS record via the Cloudflare API.
	if err := r.upsertCloudflareDNS(ctx, apiToken, relay.Spec.DNS.ZoneID, hostname, target); err != nil {
		r.setCondition(relay, ConditionDNSConfigured, metav1.ConditionFalse,
			"DNSUpdateFailed", fmt.Sprintf("failed to upsert DNS record: %v", err))
		return fmt.Errorf("upsert DNS: %w", err)
	}

	r.setCondition(relay, ConditionDNSConfigured, metav1.ConditionTrue,
		"DNSConfigured", fmt.Sprintf("DNS record for %s points to %s", hostname, target))

	return nil
}

// reconcileCredentialInjection creates or updates a Secret in the target
// namespace with the relay's connection credentials.
func (r *CodewireRelayReconciler) reconcileCredentialInjection(ctx context.Context, relay *codewire.CodewireRelay) error {
	spec := relay.Spec.CredentialInjection
	secretName := spec.SecretName
	if secretName == "" {
		secretName = "codewire-relay-creds"
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: spec.TargetNamespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		// Cross-namespace owner references are not allowed in Kubernetes,
		// so we do not set a controller reference here. Instead, we label
		// the secret so it can be identified as operator-managed.
		secret.Labels = labelsForRelay(relay)
		secret.Labels["codewire.io/source-namespace"] = relay.Namespace
		secret.Labels["codewire.io/source-name"] = relay.Name

		secret.Type = corev1.SecretTypeOpaque
		secret.StringData = map[string]string{
			"relay-url":          relay.Spec.BaseURL,
			"auth-token":         relay.Spec.AuthToken,
			"wireguard-endpoint": relay.Status.WireGuardEndpoint,
		}

		return nil
	})

	if err != nil {
		r.setCondition(relay, ConditionCredentialsInjected, metav1.ConditionFalse,
			"InjectionFailed", fmt.Sprintf("failed to inject credentials: %v", err))
		return err
	}

	r.setCondition(relay, ConditionCredentialsInjected, metav1.ConditionTrue,
		"Injected", fmt.Sprintf("Credentials injected into %s/%s", spec.TargetNamespace, secretName))

	return nil
}

// ---------------------------------------------------------------------------
// DNS helpers
// ---------------------------------------------------------------------------

// deleteDNSRecord removes the DNS record associated with this relay. Called
// during finalizer processing.
func (r *CodewireRelayReconciler) deleteDNSRecord(ctx context.Context, relay *codewire.CodewireRelay) error {
	if relay.Spec.DNS == nil {
		return nil
	}

	if relay.Spec.DNS.Provider != "cloudflare" {
		return fmt.Errorf("unsupported DNS provider: %s", relay.Spec.DNS.Provider)
	}

	apiToken, err := r.resolveSecretKeyRef(ctx, relay.Namespace, relay.Spec.DNS.APITokenSecretRef)
	if err != nil {
		return fmt.Errorf("resolve DNS API token for deletion: %w", err)
	}

	parsedURL, err := url.Parse(relay.Spec.BaseURL)
	if err != nil {
		return fmt.Errorf("invalid baseURL: %w", err)
	}
	hostname := parsedURL.Hostname()

	return r.deleteCloudflareDNS(ctx, apiToken, relay.Spec.DNS.ZoneID, hostname)
}

// resolveSecretKeyRef reads a value from a Kubernetes Secret.
func (r *CodewireRelayReconciler) resolveSecretKeyRef(ctx context.Context, namespace string, ref codewire.SecretKeyRef) (string, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: namespace}, secret); err != nil {
		return "", fmt.Errorf("get secret %s/%s: %w", namespace, ref.Name, err)
	}

	value, ok := secret.Data[ref.Key]
	if !ok {
		return "", fmt.Errorf("key %q not found in secret %s/%s", ref.Key, namespace, ref.Name)
	}

	return string(value), nil
}

// cloudflareRecord represents a single DNS record in the Cloudflare API response.
type cloudflareRecord struct {
	ID string `json:"id"`
}

// cloudflareListResponse represents the Cloudflare API list DNS records response.
type cloudflareListResponse struct {
	Result []cloudflareRecord `json:"result"`
}

// upsertCloudflareDNS creates or updates an A record in Cloudflare.
func (r *CodewireRelayReconciler) upsertCloudflareDNS(ctx context.Context, apiToken, zoneID, name, target string) error {
	httpClient := r.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}

	// First, list existing records to see if we need to update or create.
	listURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?type=A&name=%s",
		zoneID, url.QueryEscape(name))

	listReq, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return err
	}
	listReq.Header.Set("Authorization", "Bearer "+apiToken)
	listReq.Header.Set("Content-Type", "application/json")

	listResp, err := httpClient.Do(listReq)
	if err != nil {
		return fmt.Errorf("cloudflare list DNS records: %w", err)
	}
	defer listResp.Body.Close()

	if listResp.StatusCode != http.StatusOK {
		return fmt.Errorf("cloudflare list DNS records returned status %d", listResp.StatusCode)
	}

	// Parse the list response to find an existing record ID.
	var cfResp cloudflareListResponse
	if err := json.NewDecoder(listResp.Body).Decode(&cfResp); err != nil {
		return fmt.Errorf("decode cloudflare list response: %w", err)
	}

	recordID := ""
	if len(cfResp.Result) > 0 {
		recordID = cfResp.Result[0].ID
	}

	// Build the upsert request body.
	body := fmt.Sprintf(`{"type":"A","name":"%s","content":"%s","ttl":300,"proxied":false}`, name, target)

	var method, apiURL string
	if recordID != "" {
		method = http.MethodPut
		apiURL = fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s", zoneID, recordID)
	} else {
		method = http.MethodPost
		apiURL = fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records", zoneID)
	}

	upsertReq, err := http.NewRequestWithContext(ctx, method, apiURL, strings.NewReader(body))
	if err != nil {
		return err
	}
	upsertReq.Header.Set("Authorization", "Bearer "+apiToken)
	upsertReq.Header.Set("Content-Type", "application/json")

	upsertResp, err := httpClient.Do(upsertReq)
	if err != nil {
		return fmt.Errorf("cloudflare upsert DNS record: %w", err)
	}
	defer upsertResp.Body.Close()

	if upsertResp.StatusCode != http.StatusOK && upsertResp.StatusCode != http.StatusCreated {
		return fmt.Errorf("cloudflare upsert returned status %d", upsertResp.StatusCode)
	}

	return nil
}

// deleteCloudflareDNS removes an A record from Cloudflare.
func (r *CodewireRelayReconciler) deleteCloudflareDNS(ctx context.Context, apiToken, zoneID, name string) error {
	httpClient := r.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}

	// Find the record ID first.
	listURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?type=A&name=%s",
		zoneID, url.QueryEscape(name))

	listReq, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return err
	}
	listReq.Header.Set("Authorization", "Bearer "+apiToken)
	listReq.Header.Set("Content-Type", "application/json")

	listResp, err := httpClient.Do(listReq)
	if err != nil {
		return fmt.Errorf("cloudflare list DNS records for deletion: %w", err)
	}
	defer listResp.Body.Close()

	var cfResp cloudflareListResponse
	if err := json.NewDecoder(listResp.Body).Decode(&cfResp); err != nil {
		return fmt.Errorf("decode cloudflare list response: %w", err)
	}

	if len(cfResp.Result) == 0 {
		// Record doesn't exist, nothing to delete.
		return nil
	}
	recordID := cfResp.Result[0].ID

	delURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s", zoneID, recordID)
	delReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, delURL, nil)
	if err != nil {
		return err
	}
	delReq.Header.Set("Authorization", "Bearer "+apiToken)

	delResp, err := httpClient.Do(delReq)
	if err != nil {
		return fmt.Errorf("cloudflare delete DNS record: %w", err)
	}
	defer delResp.Body.Close()

	if delResp.StatusCode != http.StatusOK {
		return fmt.Errorf("cloudflare delete returned status %d", delResp.StatusCode)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Helper functions
// ---------------------------------------------------------------------------

// generateToken produces a cryptographically random alphanumeric string of
// length n.
func generateToken(n int) string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	result := make([]byte, n)
	for i := range result {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			// Fallback: this should never happen with crypto/rand.
			result[i] = charset[0]
			continue
		}
		result[i] = charset[idx.Int64()]
	}
	return string(result)
}

// setCondition sets or updates a condition on the relay's status.
func (r *CodewireRelayReconciler) setCondition(relay *codewire.CodewireRelay, condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&relay.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		ObservedGeneration: relay.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	})
}

// labelsForRelay returns the standard set of labels for resources managed by
// this operator for a given CodewireRelay instance.
func labelsForRelay(relay *codewire.CodewireRelay) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "codewire-relay",
		"app.kubernetes.io/instance":   relay.Name,
		"app.kubernetes.io/managed-by": "codewire-operator",
	}
}

// relayImage returns the container image to use for the relay. It checks the
// CR spec first, then falls back to the reconciler's configured default, and
// finally to the hardcoded default.
func (r *CodewireRelayReconciler) relayImage(relay *codewire.CodewireRelay) string {
	if relay.Spec.Image != nil {
		repo := relay.Spec.Image.Repository
		tag := relay.Spec.Image.Tag
		if repo != "" {
			if tag != "" {
				return repo + ":" + tag
			}
			return repo + ":latest"
		}
	}

	if r.Image != "" {
		return r.Image
	}

	return defaultImage
}

// SetupWithManager registers the controller with the manager.
func (r *CodewireRelayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&codewire.CodewireRelay{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&networkingv1.Ingress{}).
		Complete(r)
}

package platform

import "time"

// PlatformConfig is stored at ~/.config/cw/config.json.
type PlatformConfig struct {
	ServerURL        string `json:"server_url"`
	SessionToken     string `json:"session_token"`
	DefaultOrg       string `json:"default_org,omitempty"`
	DefaultResource  string `json:"default_resource,omitempty"`
	CoderBinary      string `json:"coder_binary,omitempty"`
	CurrentWorkspace string `json:"current_workspace,omitempty"`
}

// Auth types

type SignInRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type SignInResponse struct {
	User              *User    `json:"user,omitempty"`
	Session           *Session `json:"session,omitempty"`
	TwoFactorRequired bool     `json:"twoFactorRequired,omitempty"`
	TwoFactorToken    string   `json:"twoFactorToken,omitempty"`
}

type ValidateTOTPRequest struct {
	Code  string `json:"code"`
	Token string `json:"token"`
}

type AuthResponse struct {
	User    *User    `json:"user,omitempty"`
	Session *Session `json:"session,omitempty"`
}

type User struct {
	ID               string `json:"id"`
	Email            string `json:"email"`
	EmailVerified    bool   `json:"email_verified"`
	Name             string `json:"name,omitempty"`
	Image            string `json:"image,omitempty"`
	IsAdmin          bool   `json:"is_admin"`
	TwoFactorEnabled bool   `json:"two_factor_enabled"`
	CreatedAt        string `json:"created_at"`
}

type Session struct {
	ID        string `json:"id"`
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

// Organization types

type Organization struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Slug      string `json:"slug"`
	CreatedAt string `json:"created_at"`
}

type OrgWithRole struct {
	Organization
	Role          string             `json:"role"`
	BillingPlan   string             `json:"billingPlan,omitempty"`
	BillingStatus string             `json:"billingStatus,omitempty"`
	TrialEndsAt   *string            `json:"trialEndsAt,omitempty"`
	Resources     []ResourceSummary  `json:"resources,omitempty"`
}

type ResourceSummary struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Slug         string `json:"slug"`
	Type         string `json:"type"`
	Status       string `json:"status"`
	HealthStatus string `json:"health_status,omitempty"`
}

// Resource types

type PlatformResource struct {
	ID                string          `json:"id"`
	OrgID             string          `json:"org_id"`
	Type              string          `json:"type"`
	Name              string          `json:"name"`
	Slug              string          `json:"slug"`
	Status            string          `json:"status"`
	Config            *map[string]any `json:"config,omitempty"`
	Metadata          *map[string]any `json:"metadata,omitempty"`
	ProvisionPhase    string          `json:"provision_phase,omitempty"`
	ProvisionError    string          `json:"provision_error,omitempty"`
	HealthStatus      string          `json:"health_status"`
	HealthCheckedAt   *time.Time      `json:"health_checked_at,omitempty"`
	BillingPlan       string          `json:"billing_plan"`
	BillingStatus     string          `json:"billing_status"`
	CreatedAt         string          `json:"created_at"`
	UpdatedAt         string          `json:"updated_at"`
}

// Workspace types

type WorkspaceSummary struct {
	ID                  string  `json:"id"`
	Name                string  `json:"name"`
	OwnerName           string  `json:"owner_name"`
	Status              string  `json:"status"`
	TemplateDisplayName string  `json:"template_display_name"`
	LastUsedAt          *string `json:"last_used_at,omitempty"`
}

type WorkspacesListResponse struct {
	Workspaces []WorkspaceSummary `json:"workspaces"`
	Count      int                `json:"count"`
}

// API error

type APIError struct {
	Status  int      `json:"status"`
	Title   string   `json:"title"`
	Detail  string   `json:"detail,omitempty"`
	Errors  []string `json:"errors,omitempty"`
}

func (e *APIError) Error() string {
	if e.Detail != "" {
		return e.Detail
	}
	return e.Title
}

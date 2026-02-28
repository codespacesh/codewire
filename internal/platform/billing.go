package platform

import "fmt"

// Billing overview types (mirrors server BillingOverviewResponse)
type BillingOverview struct {
	BillingEnabled        bool          `json:"billing_enabled"`
	HasPaymentMethod      bool          `json:"has_payment_method"`
	Plan                  string        `json:"plan"`
	PlanDisplayName       string        `json:"plan_display_name"`
	Status                string        `json:"status"`
	TrialEnd              *string       `json:"trial_end"`
	SeatCount             int           `json:"seat_count"`
	IncludedDevs          int           `json:"included_devs"`
	ExtraSeatPriceCents   int           `json:"extra_seat_price_cents"`
	CurrentPeriodEnd      *string       `json:"current_period_end"`
	ActiveSubscriptions   int           `json:"active_subscriptions"`
	TotalMonthlyCostCents int           `json:"total_monthly_cost_cents"`
	CurrentMonthUsage     UsageSummary  `json:"current_month_usage"`
	Limits                BillingLimits `json:"limits"`
}

type BillingLimits struct {
	CoderInstances       LimitUsage `json:"coder_instances"`
	TeamMembers          LimitUsage `json:"team_members"`
	ConcurrentWorkspaces int        `json:"concurrent_workspaces"`
	StorageGB            int        `json:"storage_gb"`
}

type LimitUsage struct {
	Used int `json:"used"`
	Max  int `json:"max"`
}

type UsageSummary struct {
	CPUHours       float64 `json:"cpu_hours"`
	MemoryGBHours  float64 `json:"memory_gb_hours"`
	StorageGBHours float64 `json:"storage_gb_hours"`
}

// Resource usage types (mirrors server ResourceUsageResponse)
type ResourceUsage struct {
	PeriodStart   string          `json:"period_start"`
	PeriodEnd     string          `json:"period_end"`
	CPUHours      float64         `json:"cpu_hours"`
	MemoryGBHours float64         `json:"memory_gb_hours"`
	DiskGBHours   float64         `json:"disk_gb_hours"`
	Included      UsageAllowances `json:"included"`
	Overage       UsageOverage    `json:"overage"`
}

type UsageAllowances struct {
	CPUHours      int `json:"cpu_hours"`
	MemoryGBHours int `json:"memory_gb_hours"`
	DiskGBHours   int `json:"disk_gb_hours"`
}

type UsageOverage struct {
	CPUHours      float64 `json:"cpu_hours"`
	MemoryGBHours float64 `json:"memory_gb_hours"`
	DiskGBHours   float64 `json:"disk_gb_hours"`
	TotalCents    int     `json:"total_cents"`
}

// Resource billing types (mirrors server ResourceBillingResponse)
type ResourceBilling struct {
	Plan             string  `json:"plan"`
	PlanDisplayName  string  `json:"plan_display_name"`
	Status           string  `json:"status"`
	TrialEnd         *string `json:"trial_end"`
	CurrentPeriodEnd *string `json:"current_period_end"`
}

func (c *Client) GetBillingOverview(orgID string) (*BillingOverview, error) {
	var resp BillingOverview
	err := c.do("GET", fmt.Sprintf("/api/v1/organizations/%s/billing", orgID), nil, &resp)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) GetResourceUsage(resourceID string) (*ResourceUsage, error) {
	var resp ResourceUsage
	err := c.do("GET", fmt.Sprintf("/api/v1/resources/%s/usage", resourceID), nil, &resp)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) GetResourceBilling(resourceID string) (*ResourceBilling, error) {
	var resp ResourceBilling
	err := c.do("GET", fmt.Sprintf("/api/v1/resources/%s/billing", resourceID), nil, &resp)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) CreateResourceCheckout(resourceID string, req *ResourceCheckoutRequest) (*CheckoutURLResponse, error) {
	var resp CheckoutURLResponse
	err := c.do("POST", fmt.Sprintf("/api/v1/resources/%s/billing/checkout", resourceID), req, &resp)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

package platform

import "fmt"

// SecretMetadata represents a secret key with timestamps (no value exposed).
type SecretMetadata struct {
	Key       string `json:"key"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

type listSecretsResponse struct {
	Secrets []SecretMetadata `json:"secrets"`
}

type setSecretRequest struct {
	OrgID string `json:"org_id"`
	Key   string `json:"key"`
	Value string `json:"value"`
}

// ListSecrets returns secret metadata (names only, no values) for an org.
func (c *Client) ListSecrets(orgID string) ([]SecretMetadata, error) {
	var resp listSecretsResponse
	if err := c.do("GET", fmt.Sprintf("/api/v1/secrets?org_id=%s", orgID), nil, &resp); err != nil {
		return nil, err
	}
	return resp.Secrets, nil
}

// SetSecret creates or updates a secret for an org.
func (c *Client) SetSecret(orgID, key, value string) error {
	return c.do("PUT", "/api/v1/secrets", &setSecretRequest{
		OrgID: orgID,
		Key:   key,
		Value: value,
	}, nil)
}

// DeleteSecret removes a secret from an org.
func (c *Client) DeleteSecret(orgID, key string) error {
	return c.do("DELETE", fmt.Sprintf("/api/v1/secrets/%s?org_id=%s", key, orgID), nil, nil)
}

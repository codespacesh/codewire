package platform

import "fmt"

type LoginKey struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	PublicKey   string `json:"public_key"`
	Fingerprint string `json:"fingerprint"`
	CreatedAt   string `json:"created_at"`
}

func (c *Client) ListLoginKeys() ([]LoginKey, error) {
	var keys []LoginKey
	if err := c.do("GET", "/api/v1/user/login-keys", nil, &keys); err != nil {
		return nil, err
	}
	if keys == nil {
		keys = []LoginKey{}
	}
	return keys, nil
}

func (c *Client) AddLoginKey(name, publicKey string) (*LoginKey, error) {
	body := map[string]string{"name": name, "public_key": publicKey}
	var key LoginKey
	if err := c.do("POST", "/api/v1/user/login-keys", body, &key); err != nil {
		return nil, err
	}
	return &key, nil
}

func (c *Client) DeleteLoginKey(id string) error {
	return c.do("DELETE", fmt.Sprintf("/api/v1/user/login-keys/%s", id), nil, nil)
}

type SSHProxyCheck struct {
	Available bool `json:"available"`
}

func (c *Client) CheckSSHProxy(orgID, envID string) (bool, error) {
	var check SSHProxyCheck
	if err := c.do("GET", fmt.Sprintf("/api/v1/organizations/%s/environments/%s/ssh-proxy/check", orgID, envID), nil, &check); err != nil {
		return false, err
	}
	return check.Available, nil
}

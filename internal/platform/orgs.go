package platform

// ListOrgs returns all organizations the user belongs to with their roles and resources.
func (c *Client) ListOrgs() ([]OrgWithRole, error) {
	var orgs []OrgWithRole
	if err := c.do("GET", "/api/v1/organizations", nil, &orgs); err != nil {
		return nil, err
	}
	return orgs, nil
}

// GetOrg returns a single organization by ID.
func (c *Client) GetOrg(orgID string) (*OrgWithRole, error) {
	var org OrgWithRole
	if err := c.do("GET", "/api/v1/organizations/"+orgID, nil, &org); err != nil {
		return nil, err
	}
	return &org, nil
}

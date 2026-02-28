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

// CreateOrg creates a new organization.
func (c *Client) CreateOrg(req *CreateOrgRequest) (*Organization, error) {
	var org Organization
	if err := c.do("POST", "/api/v1/organizations", req, &org); err != nil {
		return nil, err
	}
	return &org, nil
}

// DeleteOrg deletes an organization by ID.
func (c *Client) DeleteOrg(orgID string) error {
	return c.do("DELETE", "/api/v1/organizations/"+orgID, nil, nil)
}

// CreateInvitation invites a member to an organization.
func (c *Client) CreateInvitation(orgID string, req *InviteMemberRequest) (*OrgInvitation, error) {
	var inv OrgInvitation
	if err := c.do("POST", "/api/v1/organizations/"+orgID+"/invitations", req, &inv); err != nil {
		return nil, err
	}
	return &inv, nil
}

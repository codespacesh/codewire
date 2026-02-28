package platform

// ListResources returns all resources the user has access to across all orgs.
func (c *Client) ListResources() ([]PlatformResource, error) {
	var resources []PlatformResource
	if err := c.do("GET", "/api/v1/resources", nil, &resources); err != nil {
		return nil, err
	}
	return resources, nil
}

// GetResource returns a single resource by ID or slug.
func (c *Client) GetResource(idOrSlug string) (*PlatformResource, error) {
	var resource PlatformResource
	if err := c.do("GET", "/api/v1/resources/"+idOrSlug, nil, &resource); err != nil {
		return nil, err
	}
	return &resource, nil
}

// CreateResource creates a new resource.
func (c *Client) CreateResource(req *CreateResourceRequest) (*CreateResourceResult, error) {
	var result CreateResourceResult
	if err := c.do("POST", "/api/v1/resources", req, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// DeleteResource deletes a resource by ID or slug.
func (c *Client) DeleteResource(idOrSlug string) error {
	return c.do("DELETE", "/api/v1/resources/"+idOrSlug, nil, nil)
}

// ListWorkspaces returns workspaces for a given resource.
func (c *Client) ListWorkspaces(resourceID string) (*WorkspacesListResponse, error) {
	var resp WorkspacesListResponse
	if err := c.do("GET", "/api/v1/resources/"+resourceID+"/workspaces", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

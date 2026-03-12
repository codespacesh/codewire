package platform

// DetectRepo calls the LLM detection endpoint for a repository URL.
func (c *Client) DetectRepo(repoURL, branch string) (*DetectionResult, error) {
	body := map[string]string{"repo_url": repoURL}
	if branch != "" {
		body["branch"] = branch
	}
	var result DetectionResult
	if err := c.do("POST", "/api/v1/launch/detect", body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

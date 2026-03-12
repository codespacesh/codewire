package platform

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// StreamProvisionEvents connects to the SSE endpoint and sends events to the channel.
// It closes the channel when the connection ends or an error occurs.
// Returns an error if the initial connection fails.
func (c *Client) StreamProvisionEvents(resourceID string, events chan<- ProvisionEvent) error {
	req, err := http.NewRequest("GET", c.ServerURL+"/api/v1/resources/"+resourceID+"/provision-events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if c.SessionToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.SessionToken)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("connect to event stream: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return fmt.Errorf("event stream returned %d", resp.StatusCode)
	}

	go func() {
		defer resp.Body.Close()
		defer close(events)

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")

			var ev ProvisionEvent
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				continue
			}
			events <- ev
		}
	}()

	return nil
}

// StreamEnvironmentLogs connects to the environment logs SSE endpoint and sends events to the channel.
// It closes the channel when the connection ends or an error occurs.
func (c *Client) StreamEnvironmentLogs(orgID, envID string, events chan<- EnvironmentLog) error {
	url := fmt.Sprintf("%s/api/v1/organizations/%s/environments/%s/logs", c.ServerURL, orgID, envID)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if c.SessionToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.SessionToken)
	}

	// Use a client without timeout for SSE streaming.
	sseClient := &http.Client{}
	resp, err := sseClient.Do(req)
	if err != nil {
		return fmt.Errorf("connect to log stream: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return fmt.Errorf("log stream returned %d", resp.StatusCode)
	}

	go func() {
		defer resp.Body.Close()
		defer close(events)

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")

			var ev EnvironmentLog
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				continue
			}
			events <- ev
		}
	}()

	return nil
}

// GetEnvironmentLogs fetches all environment logs (JSON fallback).
func (c *Client) GetEnvironmentLogs(orgID, envID string) ([]EnvironmentLog, error) {
	var logs []EnvironmentLog
	path := fmt.Sprintf("/api/v1/organizations/%s/environments/%s/logs", orgID, envID)
	if err := c.do("GET", path, nil, &logs); err != nil {
		return nil, err
	}
	return logs, nil
}

// GetProvisionEvents fetches all provision events for a resource (JSON fallback).
func (c *Client) GetProvisionEvents(resourceID string) ([]ProvisionEvent, error) {
	var events []ProvisionEvent
	if err := c.do("GET", "/api/v1/resources/"+resourceID+"/provision-events", nil, &events); err != nil {
		return nil, err
	}
	return events, nil
}

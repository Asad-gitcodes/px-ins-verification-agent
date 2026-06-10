package api

import "fmt"

type Client struct{}

func NewClient() *Client {
	return &Client{}
}

func (c *Client) FetchMemberData() error {
	// This boundary is for DentaQuest API-backed data collection once the
	// browser login/session pieces can provide whatever tokens are required.
	return fmt.Errorf("TODO: port DentaQuest Max API-backed collection")
}

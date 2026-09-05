package protocol

import (
	"context"
	"fmt"
	"slices"
	"time"

	"akid/internal/model"
)

type ApplyParams struct {
	Processes []model.ProcessConfig `json:"processes"`
}

type Capabilities struct {
	Protocol       int      `json:"protocol"`
	Methods        []string `json:"methods"`
	Features       []string `json:"features"`
	MaxMessageSize int      `json:"max_message_size"`
}

func SupportedCapabilities() Capabilities {
	return Capabilities{
		Protocol:       Version,
		Methods:        []string{"daemon.ping", "daemon.capabilities", "daemon.shutdown", "config.apply", "process.create", "process.list", "process.get", "process.metrics", "process.start", "process.stop", "process.restart", "process.delete", "event.subscribe", "log.read", "log.subscribe"},
		Features:       []string{"process.snapshot", "process.deleted", "process.revision", "log.gap", "log.partial", "subscription.lagged"},
		MaxMessageSize: MaxMessageSize,
	}
}

func (c *Client) RequireCapabilities(parent context.Context, methods ...string) error {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	var supported Capabilities
	if err := c.Call(ctx, "daemon.capabilities", nil, &supported); err != nil {
		return fmt.Errorf("query daemon capabilities (a daemon upgrade may be needed): %w", err)
	}
	if supported.Protocol != Version {
		return fmt.Errorf("unsupported daemon protocol %d", supported.Protocol)
	}
	for _, method := range methods {
		if !slices.Contains(supported.Methods, method) {
			return fmt.Errorf("daemon does not support %s; upgrade the daemon", method)
		}
	}
	return nil
}

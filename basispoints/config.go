package basispoints

import (
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	pluginName          = "basispoints"
	pluginVersion       = "0.1.0"
	defaultBaseURL      = "https://bps.openai.com/basispoints/api"
	defaultAuthMode     = "chatgpt"
	defaultAuthProvider = "codex"
	defaultMaxEffort    = "high"
	defaultMaxInFlight  = 1
	defaultCooldown     = 30 * time.Second
	providerCodex       = "codex"
	formatCodex         = "codex"
)

var defaultModels = []string{"gpt-6-astra", "gpt-5.6-sol"}

var effortRank = map[string]int{
	"none":    0,
	"minimal": 1,
	"low":     2,
	"medium":  3,
	"high":    4,
	"xhigh":   5,
	"max":     6,
}

// Config is the runtime snapshot read from plugins.configs.basispoints.
type Config struct {
	BaseURL      string
	AuthMode     string
	AuthProvider string
	Models       map[string]string
	MaxEffort    string
	MaxInFlight  int
	MinInterval  time.Duration
	Cooldown     time.Duration
}

type rawConfig struct {
	BaseURL      string   `yaml:"base_url"`
	AuthMode     string   `yaml:"auth_mode"`
	AuthProvider string   `yaml:"auth_provider"`
	Models       []string `yaml:"models"`
	MaxEffort    string   `yaml:"max_effort"`
	MaxInFlight  int      `yaml:"max_in_flight_per_account"`
	MinInterval  int      `yaml:"min_interval_ms"`
	Cooldown     int      `yaml:"cooldown_ms"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ModelProvider         bool     `json:"model_provider"`
	ModelRouter           bool     `json:"model_router"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
}

func defaultConfig() Config {
	return normalize(rawConfig{})
}

func parseConfig(raw []byte) (Config, error) {
	var decoded rawConfig
	if len(raw) > 0 {
		if errUnmarshal := yaml.Unmarshal(raw, &decoded); errUnmarshal != nil {
			return Config{}, errUnmarshal
		}
	}
	return normalize(decoded), nil
}

func normalize(raw rawConfig) Config {
	cfg := Config{
		BaseURL:      strings.TrimRight(strings.TrimSpace(raw.BaseURL), "/"),
		AuthMode:     strings.TrimSpace(raw.AuthMode),
		AuthProvider: strings.ToLower(strings.TrimSpace(raw.AuthProvider)),
		MaxEffort:    strings.ToLower(strings.TrimSpace(raw.MaxEffort)),
		MaxInFlight:  raw.MaxInFlight,
		Models:       map[string]string{},
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	if cfg.AuthMode == "" {
		cfg.AuthMode = defaultAuthMode
	}
	if cfg.AuthProvider == "" {
		cfg.AuthProvider = defaultAuthProvider
	}
	if _, ok := effortRank[cfg.MaxEffort]; !ok {
		cfg.MaxEffort = defaultMaxEffort
	}
	if cfg.MaxInFlight <= 0 {
		cfg.MaxInFlight = defaultMaxInFlight
	}
	if raw.MinInterval > 0 {
		cfg.MinInterval = time.Duration(raw.MinInterval) * time.Millisecond
	}
	if raw.Cooldown > 0 {
		cfg.Cooldown = time.Duration(raw.Cooldown) * time.Millisecond
	} else {
		cfg.Cooldown = defaultCooldown
	}
	models := raw.Models
	if len(models) == 0 {
		models = defaultModels
	}
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		cfg.Models[strings.ToLower(model)] = model
	}
	return cfg
}

func (c Config) canonicalModel(model string) (string, bool) {
	base, _, _ := splitModelSuffix(model)
	canonical, ok := c.Models[strings.ToLower(strings.TrimSpace(base))]
	return canonical, ok
}

func (c Config) modelInfos() []pluginapi.ModelInfo {
	if len(c.Models) == 0 {
		return nil
	}
	names := make([]string, 0, len(c.Models))
	for _, name := range c.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	models := make([]pluginapi.ModelInfo, 0, len(names))
	for _, name := range names {
		models = append(models, pluginapi.ModelInfo{
			ID:          name,
			Object:      "model",
			OwnedBy:     "openai",
			Type:        "chat",
			DisplayName: name,
			Name:        name,
			Description: "Basis Points responses model",
			Thinking: &pluginapi.ThinkingSupport{
				Levels: []string{"minimal", "low", "medium", "high"},
			},
			SupportedGenerationMethods: []string{"responses", "chat"},
			UserDefined:                true,
		})
	}
	return models
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "wsure",
			GitHubRepository: "https://github.com/router-for-me/CLIProxyAPI",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "base_url", Type: pluginapi.ConfigFieldTypeString, Description: "Basis Points API origin. The plugin appends /responses."},
				{Name: "auth_mode", Type: pluginapi.ConfigFieldTypeString, Description: "Value of x-basispoints-auth-mode. Defaults to chatgpt."},
				{Name: "auth_provider", Type: pluginapi.ConfigFieldTypeString, Description: "Host credential provider to read tokens from. Defaults to codex."},
				{Name: "models", Type: pluginapi.ConfigFieldTypeArray, Description: "Model ids routed to Basis Points. Defaults to gpt-6-astra and gpt-5.6-sol."},
				{Name: "max_effort", Type: pluginapi.ConfigFieldTypeString, Description: "Highest reasoning.effort sent upstream. max and xhigh clamp down to this value. Defaults to high."},
				{Name: "max_in_flight_per_account", Type: pluginapi.ConfigFieldTypeInteger, Description: "Simultaneous upstream requests per Codex account. Defaults to 1."},
				{Name: "min_interval_ms", Type: pluginapi.ConfigFieldTypeInteger, Description: "Minimum gap between requests on the same account. Defaults to 0."},
				{Name: "cooldown_ms", Type: pluginapi.ConfigFieldTypeInteger, Description: "Local cooldown after HTTP 429 when Retry-After is absent. Defaults to 30000."},
			},
		},
		Capabilities: registrationCapability{
			ModelProvider:         true,
			ModelRouter:           true,
			Executor:              true,
			ExecutorModelScope:    string(pluginapi.ExecutorModelScopeStatic),
			ExecutorInputFormats:  []string{formatCodex},
			ExecutorOutputFormats: []string{formatCodex},
		},
	}
}

func splitModelSuffix(model string) (string, string, bool) {
	model = strings.TrimSpace(model)
	if !strings.HasSuffix(model, ")") {
		return model, "", false
	}
	open := strings.LastIndex(model, "(")
	if open <= 0 {
		return model, "", false
	}
	base := strings.TrimSpace(model[:open])
	suffix := strings.TrimSpace(model[open+1 : len(model)-1])
	if base == "" || suffix == "" {
		return model, "", false
	}
	return base, suffix, true
}

func clampEffort(effort, maxEffort string) string {
	effort = strings.ToLower(strings.TrimSpace(effort))
	maxEffort = strings.ToLower(strings.TrimSpace(maxEffort))
	if effort == "" {
		return ""
	}
	maxRank, okMax := effortRank[maxEffort]
	if !okMax {
		maxRank = effortRank[defaultMaxEffort]
		maxEffort = defaultMaxEffort
	}
	rank, ok := effortRank[effort]
	if !ok {
		return maxEffort
	}
	if rank > maxRank {
		return maxEffort
	}
	return effort
}

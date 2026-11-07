package config

// Config is the top‑level configuration struct.
type Config struct {
	Root       string                     `json:"-"`
	System     SystemConfig               `json:"system"`
	Admin      AdminConfig                `json:"admin"`
	Maintainer MaintainerConfig           `json:"maintainer"`
	Users      map[string]UserConfig      `json:"users"` // key: phone, e.g. "+15555550102"
	Satellites map[string]SatelliteConfig `json:"satellites"`
	Routes     RoutesConfig               `json:"routes"`
}

type SystemConfig struct {
	Server          ServerConfig              `json:"server"`
	Persona         PersonaConfig             `json:"persona"`
	Auth            AuthConfig                `json:"auth"`
	Limits          LimitsConfig              `json:"limits"`
	DefaultProvider string                    `json:"default_provider"`
	Providers       map[string]ProviderConfig `json:"providers"`
	Schedules       []ScheduleConfig          `json:"schedules"`
}

type ServerConfig struct {
	Port   int    `json:"port"`
	DBPath string `json:"db_path"`
}

type PersonaConfig struct {
	Name     string   `json:"name"`
	Prefixes []string `json:"prefixes"`
}

type AuthConfig struct {
	JWTSecret    string `json:"jwt_secret"`
	IngressToken string `json:"ingress_token,omitempty"`
}

type LimitsConfig struct {
	RatePerSecond float64 `json:"rate_per_second"`
	Burst         int     `json:"burst"`
	QueueDepth    int     `json:"queue_depth"`
	// DispatchQueueDepth bounds the inbound dispatch queue. Zero means the
	// dispatch default (256).
	DispatchQueueDepth int `json:"dispatch_queue_depth"`
	// SendTimeoutSeconds bounds one outbound send attempt. Zero means the
	// queue default (10s).
	SendTimeoutSeconds int `json:"send_timeout_seconds"`
	// DedupeTTLSeconds bounds how long an inbound (sender, message_id) is
	// remembered. Zero means the store default (900s).
	DedupeTTLSeconds int `json:"dedupe_ttl_seconds"`
}

type ProviderConfig struct {
	Type    string `json:"type"`
	Account string `json:"account,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
	DBPath  string `json:"db_path,omitempty"`
}

type ScheduleConfig struct {
	Name       string         `json:"name"`
	Cron       string         `json:"cron"`
	Satellite  string         `json:"satellite"`
	Command    string         `json:"command"`
	Args       map[string]any `json:"args,omitempty"`
	Recipients []string       `json:"recipients,omitempty"`
}

type AdminConfig struct {
	Admins          []string `json:"admins"`
	ApprovedNumbers []string `json:"approved_numbers"`
	DeniedNumbers   []string `json:"denied_numbers"`
	ApprovedGroups  []string `json:"approved_groups"`
}

type MaintainerConfig struct {
	Maintainers     []string `json:"maintainers"`
	LastAssigned    int      `json:"last_assigned"`
	NotifyApprovals bool     `json:"notify_approvals"`
}

type UserConfig struct {
	Prefixes     []string          `json:"prefixes"`
	Timezone     string            `json:"timezone"`
	Muted        bool              `json:"muted"`
	SatelliteIDs map[string]string `json:"satellite_ids"`
	Aliases      Aliases           `json:"aliases"`
}

type Aliases struct {
	Satellites map[string]string `json:"satellites"`
	Commands   map[string]string `json:"commands"`
}

type SatelliteConfig struct {
	Name     string          `json:"name"`
	Trigger  string          `json:"trigger"`
	Endpoint string          `json:"endpoint"`
	Commands []CommandConfig `json:"commands"`
}

type CommandConfig struct {
	Name     string   `json:"name"`
	Path     string   `json:"path"`
	Patterns []string `json:"patterns"`
	Args     []ArgDef `json:"args,omitempty"`
}

type ArgDef struct {
	Name string   `json:"name"`
	Type string   `json:"type"`
	Enum []string `json:"enum,omitempty"`
}

type RoutesConfig struct {
	Routes []RouteEntry `json:"routes"`
}

type RouteEntry struct {
	Trigger   string          `json:"trigger"`
	Satellite string          `json:"satellite"`
	Endpoint  string          `json:"endpoint"`
	Commands  []CommandConfig `json:"commands"`
}

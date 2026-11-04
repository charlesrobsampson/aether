package deploy_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// composeDocument is the subset of docker-compose.yml this test validates.
type composeDocument struct {
	Services map[string]composeService `yaml:"services"`
}

type composeService struct {
	Image       string              `yaml:"image"`
	Build       yaml.Node           `yaml:"build"`
	Command     []string            `yaml:"command"`
	User        string              `yaml:"user"`
	Environment map[string]string   `yaml:"environment"`
	DependsOn   []string            `yaml:"depends_on"`
	Healthcheck *composeHealthcheck `yaml:"healthcheck"`
}

type composeHealthcheck struct {
	Test        []string `yaml:"test"`
	Interval    string   `yaml:"interval"`
	Timeout     string   `yaml:"timeout"`
	Retries     int      `yaml:"retries"`
	StartPeriod string   `yaml:"start_period"`
}

// TestComposeConfig guards the packaging contract in plan §3.18: local compose
// starts Aether with a healthcheck and a non-root user, alongside the msg
// sidecar; the Dockerfile is multi-stage and the runtime stage is Alpine rather
// than scratch so CA certificates and zoneinfo are available.
func TestComposeConfig(t *testing.T) {
	root := repoRoot(t)

	t.Run("services", func(t *testing.T) {
		doc := loadCompose(t, root)

		aether, ok := doc.Services["aether"]
		if !ok {
			t.Fatal("docker-compose.yml has no aether service")
		}
		msg, ok := doc.Services["msg"]
		if !ok {
			t.Fatal("docker-compose.yml has no msg sidecar")
		}

		if aether.Healthcheck == nil {
			t.Fatal("aether service has no healthcheck")
		}
		if len(aether.Healthcheck.Test) == 0 {
			t.Error("aether healthcheck has no test command")
		}
		if aether.Healthcheck.Interval == "" || aether.Healthcheck.Timeout == "" {
			t.Error("aether healthcheck must set interval and timeout")
		}
		if aether.Healthcheck.Retries < 1 {
			t.Errorf("aether healthcheck retries = %d, want at least 1", aether.Healthcheck.Retries)
		}
		if !mentionsHealthz(aether.Healthcheck.Test) {
			t.Errorf("aether healthcheck test = %v, want a /healthz probe", aether.Healthcheck.Test)
		}

		if !isNonRootUser(aether.User) {
			t.Errorf("aether user = %q, want a non-root user", aether.User)
		}

		if !containsString(aether.DependsOn, "msg") {
			t.Errorf("aether depends_on = %v, want msg", aether.DependsOn)
		}

		command := strings.Join(msg.Command, " ")
		if !strings.Contains(command, "signal-receiver") {
			t.Errorf("msg command = %q, want a signal-receiver", command)
		}
		if !strings.Contains(command, "--forward") || !strings.Contains(command, "http://aether:8080/receive") {
			t.Errorf("msg command = %q, want --forward http://aether:8080/receive", command)
		}
	})

	t.Run("dockerfile", func(t *testing.T) {
		path := filepath.Join(root, "Dockerfile")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read Dockerfile: %v", err)
		}
		dockerfile := string(data)

		stages := dockerStages(dockerfile)
		if len(stages) < 2 {
			t.Fatalf("Dockerfile stages = %v, want a multi-stage build", stages)
		}

		final := stages[len(stages)-1]
		if base := imageBase(final); base != "alpine" {
			t.Fatalf("final Dockerfile stage = %q, want an alpine base (not scratch)", final)
		}

		runtimeStage := finalStage(dockerfile)
		for _, pkg := range []string{"ca-certificates", "tzdata"} {
			if !strings.Contains(runtimeStage, pkg) {
				t.Errorf("runtime stage does not install %s", pkg)
			}
		}
		if !hasNonRootUser(runtimeStage) {
			t.Error("runtime stage must declare a non-root USER")
		}
	})
}

func repoRoot(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root, err := filepath.Abs(filepath.Join(filepath.Dir(file), "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	return root
}

func loadCompose(t *testing.T, root string) composeDocument {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(root, "docker-compose.yml"))
	if err != nil {
		t.Fatalf("read docker-compose.yml: %v", err)
	}
	var doc composeDocument
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse docker-compose.yml: %v", err)
	}
	if len(doc.Services) == 0 {
		t.Fatal("docker-compose.yml defines no services")
	}
	return doc
}

func mentionsHealthz(test []string) bool {
	for _, part := range test {
		if strings.Contains(part, "/healthz") {
			return true
		}
	}
	return false
}

func isNonRootUser(user string) bool {
	user = strings.TrimSpace(user)
	if user == "" {
		return false
	}
	name := user
	if i := strings.IndexByte(name, ':'); i >= 0 {
		name = name[:i]
	}
	switch strings.ToLower(name) {
	case "root", "0":
		return false
	default:
		return !strings.HasPrefix(name, "0")
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// dockerStages returns the base image of every FROM instruction in order.
func dockerStages(dockerfile string) []string {
	var stages []string
	for _, line := range strings.Split(dockerfile, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 || !strings.EqualFold(fields[0], "FROM") {
			continue
		}
		for i := 1; i < len(fields); i++ {
			if strings.HasPrefix(fields[i], "--") {
				continue
			}
			stages = append(stages, fields[i])
			break
		}
	}
	return stages
}

// finalStage returns the text of the Dockerfile from its last FROM onward.
func finalStage(dockerfile string) string {
	lines := strings.Split(dockerfile, "\n")
	start := 0
	for i, line := range lines {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 && strings.EqualFold(fields[0], "FROM") {
			start = i
		}
	}
	return strings.Join(lines[start:], "\n")
}

func imageBase(image string) string {
	image = strings.ToLower(strings.TrimSpace(image))
	if i := strings.IndexByte(image, ':'); i >= 0 {
		image = image[:i]
	}
	if i := strings.LastIndexByte(image, '/'); i >= 0 {
		image = image[i+1:]
	}
	return image
}

func hasNonRootUser(stage string) bool {
	for _, line := range strings.Split(stage, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 && strings.EqualFold(fields[0], "USER") {
			return isNonRootUser(fields[1])
		}
	}
	return false
}

package dependencytest

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const module = "github.com/wago-org/http"

func TestRootDoesNotDirectlyImportProtocolTransports(t *testing.T) {
	imports := goList(t, "-f", "{{join .Imports \"\\n\"}}", module)
	for _, forbidden := range []string{"github.com/wago-org/net/tcp", "github.com/wago-org/net/udp"} {
		if linePresent(imports, forbidden) {
			t.Fatalf("root directly imports %s", forbidden)
		}
	}
}

func TestSelectivePackagesOmitOtherTransportGraphs(t *testing.T) {
	tests := []struct {
		pkg       string
		forbidden []string
	}{
		{pkg: module + "/http", forbidden: []string{"github.com/wago-org/net/udp", "github.com/wago-org/net/internal/backend/lneto/udp", "github.com/wago-org/net/internal/binding/udp"}},
		{pkg: module + "/http2", forbidden: []string{"github.com/wago-org/net/udp", "github.com/wago-org/net/internal/backend/lneto/udp", "github.com/wago-org/net/internal/binding/udp"}},
		{pkg: module + "/websocket", forbidden: []string{"github.com/wago-org/net/udp", "github.com/wago-org/net/internal/backend/lneto/udp", "github.com/wago-org/net/internal/binding/udp"}},
		{pkg: module + "/http3", forbidden: []string{"github.com/wago-org/net/tcp", "github.com/wago-org/net/internal/backend/lneto/tcp", "github.com/wago-org/net/internal/binding/tcp"}},
	}
	for _, test := range tests {
		t.Run(test.pkg, func(t *testing.T) {
			dependencies := goList(t, "-deps", test.pkg)
			for _, forbidden := range test.forbidden {
				if linePresent(dependencies, forbidden) {
					t.Fatalf("dependency graph contains %s", forbidden)
				}
			}
		})
	}
}

func goList(t *testing.T, arguments ...string) string {
	t.Helper()
	command := exec.Command("go", append([]string{"list"}, arguments...)...)
	command.Env = append(os.Environ(), "GOWORK=off")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(append([]string{"list"}, arguments...), " "), err, output)
	}
	return string(output)
}

func linePresent(output, value string) bool {
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == value {
			return true
		}
	}
	return false
}

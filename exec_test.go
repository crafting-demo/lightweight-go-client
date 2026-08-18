package crafting

import (
	"encoding/base64"
	"strings"
	"testing"
)

// The sentinel must reproduce the command's stdout byte for byte, because
// callers parse it. In particular the wrapper's own leading newline must not be
// mistaken for output, and a trailing newline that the command really did emit
// must survive.
func TestParseSentinel(t *testing.T) {
	tests := []struct {
		name       string
		stdout     string
		wantOutput string
		wantCode   int
		wantFound  bool
	}{
		{
			name:       "output without trailing newline",
			stdout:     "hello\n__CS_EXIT__0__\n",
			wantOutput: "hello",
			wantCode:   0,
			wantFound:  true,
		},
		{
			name:       "output with trailing newline is preserved",
			stdout:     "line1\n\n__CS_EXIT__0__\n",
			wantOutput: "line1\n",
			wantCode:   0,
			wantFound:  true,
		},
		{
			name:       "empty output",
			stdout:     "\n__CS_EXIT__0__\n",
			wantOutput: "",
			wantCode:   0,
			wantFound:  true,
		},
		{
			name:       "non-zero exit",
			stdout:     "partial\n__CS_EXIT__42__\n",
			wantOutput: "partial",
			wantCode:   42,
			wantFound:  true,
		},
		{
			name:       "multiline output",
			stdout:     "a\nb\nc\n__CS_EXIT__7__\n",
			wantOutput: "a\nb\nc",
			wantCode:   7,
			wantFound:  true,
		},
		{
			name:       "output that itself mentions the sentinel uses the last one",
			stdout:     "echo \n__CS_EXIT__1__\n\n__CS_EXIT__0__\n",
			wantOutput: "echo \n__CS_EXIT__1__\n",
			wantCode:   0,
			wantFound:  true,
		},
		{
			name:      "missing sentinel means the command never completed",
			stdout:    "partial output with no sentinel",
			wantFound: false,
		},
		{
			name:      "truncated sentinel is not trusted",
			stdout:    "out\n__CS_EXIT__12",
			wantFound: false,
		},
		{
			name:      "non-numeric sentinel is not trusted",
			stdout:    "out\n__CS_EXIT__abc__\n",
			wantFound: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			output, code, found := parseSentinel(tc.stdout)
			if found != tc.wantFound {
				t.Fatalf("found = %v, want %v", found, tc.wantFound)
			}
			if !tc.wantFound {
				return
			}
			if output != tc.wantOutput {
				t.Errorf("output = %q, want %q", output, tc.wantOutput)
			}
			if code != tc.wantCode {
				t.Errorf("exit code = %d, want %d", code, tc.wantCode)
			}
		})
	}
}

// Test doubles build fake CLI output with ExitSentinel, so what it writes must
// be exactly what the parser reads.
func TestExitSentinelRoundTripsThroughTheParser(t *testing.T) {
	output, code, found := parseSentinel("hello" + ExitSentinel(3))
	if !found {
		t.Fatal("the parser did not recognise its own sentinel")
	}
	if output != "hello" || code != 3 {
		t.Errorf("parsed %q, %d; want hello, 3", output, code)
	}
}

// Commands reach the sandbox base64-encoded, which keeps multi-line input and
// every flavor of quoting intact in transit.
func TestEncodeCommandSurvivesRoundTrip(t *testing.T) {
	commands := []string{
		"echo hello",
		"echo line1\necho line2\nexit 5",
		`echo "he said \"hi\""; echo 'single $NOPE'`,
		"cat <<EOF\nnested \"quotes\" and $dollar\nEOF",
		"",
	}
	for _, cmd := range commands {
		encoded := encodeCommand(cmd)
		for _, r := range encoded {
			if r == '\n' || r == '\r' {
				t.Fatalf("encoded command for %q contains a newline", cmd)
			}
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatalf("decoding %q: %v", cmd, err)
		}
		if string(decoded) != cmd {
			t.Errorf("round trip = %q, want %q", decoded, cmd)
		}
	}
}

func TestWriteFileScriptQuotesHeredocDelimiter(t *testing.T) {
	script := writeFileScript("/home/owner/.snapshot/includes.txt", ".")
	wantFragments := []string{
		`mkdir -p "$(dirname "/home/owner/.snapshot/includes.txt")"`,
		`cat > "/home/owner/.snapshot/includes.txt" <<'CRAFTING_EOF'`,
		".\nCRAFTING_EOF\n",
	}
	for _, want := range wantFragments {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q\ngot:\n%s", want, script)
		}
	}
}

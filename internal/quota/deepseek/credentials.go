package deepseek

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzstar101/mytoken/internal/quota/plan"
)

const (
	// defaultDSHHome is the DeepSeek Harness data directory under $HOME.
	defaultDSHHome = ".dsh"
	// credentialFileName is the versioned credential store DSH maintains.
	credentialFileName = ".credentials.yaml"
	// apiKeyRef is the refs entry DSH resolves DEEPSEEK_API_KEY from.
	apiKeyRef = "DEEPSEEK_API_KEY"
	// apiKeyEnv is the fallback process environment variable.
	apiKeyEnv = "DEEPSEEK_API_KEY"
	// maxCredentialBytes bounds the credential file read.
	maxCredentialBytes = 256 << 10
)

// credentialPath returns DSH's credential store: an explicit override, else
// $DSH_HOME/.credentials.yaml, else ~/.dsh/.credentials.yaml.
func (p *Provider) credentialPath() string {
	if p.pathOverride != "" {
		return p.pathOverride
	}
	home := strings.TrimSpace(p.getenv("DSH_HOME"))
	if home == "" {
		home = filepath.Join(p.home, defaultDSHHome)
	}
	return filepath.Join(home, credentialFileName)
}

// credentials returns the DeepSeek platform API key. DSH's stored key wins
// over DEEPSEEK_API_KEY in the environment. DSH's signed-in account grant is
// a platform.deepseek.com session token, not a platform API key, so it is not
// used here.
func (p *Provider) credentials() (string, error) {
	var sourceErr error
	if raw, err := readBounded(p.credentialPath(), maxCredentialBytes); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			sourceErr = err
		}
	} else if key := cleanKey(refsFromCredentials(raw)[apiKeyRef]); key != "" {
		return key, nil
	}
	if key := cleanKey(p.getenv(apiKeyEnv)); key != "" {
		return key, nil
	}
	if sourceErr != nil {
		return "", plan.NewError(plan.ReasonSourceUnreadable, sourceErr)
	}
	return "", plan.NewError(plan.ReasonNoCredentials, nil)
}

// readBounded reads at most limit bytes and rejects a larger file.
func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errors.New("deepseek: credential file is too large")
	}
	return raw, nil
}

// cleanKey trims whitespace, a Bearer prefix, and surrounding quotes. It
// rejects a value with control characters or spaces, which cannot be a valid
// Authorization header token.
func cleanKey(raw string) string {
	key := strings.TrimSpace(raw)
	if len(key) >= len("Bearer ") && strings.EqualFold(key[:len("Bearer ")], "Bearer ") {
		key = strings.TrimSpace(key[len("Bearer "):])
	}
	if len(key) >= 2 {
		if (key[0] == '"' && key[len(key)-1] == '"') || (key[0] == '\'' && key[len(key)-1] == '\'') {
			key = key[1 : len(key)-1]
		}
	}
	for _, r := range key {
		if r <= ' ' || r == 0x7f {
			return ""
		}
	}
	return key
}

// yamlLine is one non-blank, non-comment line of the credential store: its
// indent, mapping key and scalar value.
type yamlLine struct {
	indent int
	key    string
	value  string
}

func yamlLines(raw []byte) []yamlLine {
	var out []yamlLine
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		trimmed := strings.TrimLeft(line, " ")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(trimmed)
		if strings.HasPrefix(trimmed, "- ") {
			// A list item's first key sits two columns in.
			trimmed = trimmed[2:]
			indent += 2
		}
		k, v, ok := strings.Cut(trimmed, ":")
		if !ok {
			out = append(out, yamlLine{indent: indent, value: yamlScalar(trimmed)})
			continue
		}
		out = append(out, yamlLine{indent: indent, key: strings.TrimSpace(k), value: yamlScalar(v)})
	}
	return out
}

// yamlScalar unquotes a plain or quoted scalar and drops a trailing comment.
func yamlScalar(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || strings.HasPrefix(v, "#") {
		return ""
	}
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
		if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
			return v[1 : end+1]
		}
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v
}

// refsFromCredentials returns the scalar children of the top-level refs
// mapping. Multi-line block scalars are ignored; an API key is a one-line
// scalar.
func refsFromCredentials(raw []byte) map[string]string {
	out := map[string]string{}
	lines := yamlLines(raw)
	for i, l := range lines {
		if l.indent != 0 || l.key != "refs" || l.value != "" {
			continue
		}
		child := -1
		for _, c := range lines[i+1:] {
			if c.indent == 0 {
				break
			}
			if child < 0 {
				child = c.indent
			}
			if c.indent != child || c.key == "" || c.value == "" {
				continue
			}
			if c.value == "|" || c.value == ">" {
				continue
			}
			out[c.key] = c.value
		}
		break
	}
	return out
}

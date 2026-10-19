package glm

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzstar101/mytoken/internal/quota/plan"
)

// Regions the Coding Plan quota endpoint is served from.
const (
	RegionGlobal   = "global"
	RegionBigModel = "bigmodel-cn"
)

// globalBaseURL serves the international z.ai accounts.
const globalBaseURL = "https://api.z.ai"

// bigModelBaseURL serves mainland-China BigModel accounts.
const bigModelBaseURL = "https://open.bigmodel.cn"

// cnEnvKeys are the environment variables BigModel documents for its API key.
var cnEnvKeys = []string{"BIGMODEL_API_KEY", "ZHIPU_API_KEY", "ZHIPUAI_API_KEY", "GLM_API_KEY"}

// cnKeyFiles are one-line key files written by BigModel/GLM tooling.
var cnKeyFiles = []string{
	filepath.Join(".coding-relay", "glm-api-key"),
	filepath.Join(".config", "bigmodel", "api_key"),
	filepath.Join(".config", "zhipu", "api_key"),
}

// maxKeyBytes bounds a key file read.
const maxKeyBytes = 4 << 10

// credentials returns the API key for region and the region it belongs to.
func (p *Provider) credentials() (string, string, error) {
	if p.region != "" {
		token, err := p.keyFor(p.region)
		return token, p.region, err
	}
	if token, err := p.keyFor(RegionGlobal); err == nil {
		return token, RegionGlobal, nil
	}
	if token, err := p.keyFor(RegionBigModel); err == nil {
		return token, RegionBigModel, nil
	}
	return "", "", plan.NewError(plan.ReasonNoCredentials, nil)
}

func (p *Provider) keyFor(region string) (string, error) {
	if region == RegionGlobal {
		if token := cleanKey(p.getenv("Z_AI_API_KEY")); token != "" {
			return token, nil
		}
		return "", plan.NewError(plan.ReasonNoCredentials, nil)
	}
	for _, key := range cnEnvKeys {
		if token := cleanKey(p.getenv(key)); token != "" {
			return token, nil
		}
	}
	for _, name := range cnKeyFiles {
		path := filepath.Join(p.home, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return "", plan.NewError(plan.ReasonSourceUnreadable, err)
		}
		if len(raw) > maxKeyBytes {
			raw = raw[:maxKeyBytes]
		}
		if token := cleanKey(string(raw)); token != "" {
			return token, nil
		}
	}
	return "", plan.NewError(plan.ReasonNoCredentials, nil)
}

// cleanKey trims whitespace, a Bearer prefix, and surrounding quotes.
func cleanKey(raw string) string {
	key := strings.TrimSpace(raw)
	key = strings.TrimPrefix(key, "Bearer ")
	key = strings.TrimSpace(key)
	if len(key) >= 2 && key[0] == '"' && key[len(key)-1] == '"' {
		key = strings.Trim(key, `"`)
	}
	return key
}

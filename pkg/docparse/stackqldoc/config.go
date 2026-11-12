package stackqldoc

import (
	"embed"
	"fmt"
	"slices"

	"gopkg.in/yaml.v3"
)

// config is shared by every document hierarchy level. raw preserves presence
// (including false, zero and null) and lets inheritance cover every member.
type config struct {
	Retry                    *retrySpec           `yaml:"retry"`
	Views                    map[string]*viewSpec `yaml:"views"`
	RequestTranslate         algorithmSpec        `yaml:"requestTranslate"`
	RequestBodyTranslate     algorithmSpec        `yaml:"requestBodyTranslate"`
	QueryParamTranspose      algorithmSpec        `yaml:"queryParamTranspose"`
	Pagination               paginationSpec       `yaml:"pagination"`
	QueryParamPushdown       *pushdownSpec        `yaml:"queryParamPushdown"`
	SnakeCaseAliases         bool                 `yaml:"snake_case_aliases"`
	Auth                     *authConfig          `yaml:"auth"`
	Lifecycle                *lifecycleSpec       `yaml:"lifecycle"`
	raw                      map[string]any
	providerDefaultsResolved bool
}

func (c *config) UnmarshalYAML(n *yaml.Node) error {
	type plain config
	var decoded plain
	if err := n.Decode(&decoded); err != nil {
		return err
	}
	*c = config(decoded)
	return n.Decode(&c.raw)
}

type lifecycleSpec struct {
	StateDerivation string            `yaml:"state_derivation"`
	Interval        string            `yaml:"interval"`
	MaxAttempts     int               `yaml:"max_attempts"`
	Bind            map[string]string `yaml:"bind"`
}

//go:embed defaults/*.yaml
var defaultFiles embed.FS

type configDefault struct {
	Path    []string       `yaml:"path"`
	Config  map[string]any `yaml:"config"`
	decoded *config
}

var configDefaults = func() []configDefault {
	entries, err := defaultFiles.ReadDir("defaults")
	if err != nil {
		panic(err)
	}
	var out []configDefault
	for _, entry := range entries {
		data, err := defaultFiles.ReadFile("defaults/" + entry.Name())
		if err != nil {
			panic(err)
		}
		var declarations []configDefault
		if err := yaml.Unmarshal(data, &declarations); err != nil {
			panic(err)
		}
		for index := range declarations {
			encoded, err := yaml.Marshal(declarations[index].Config)
			if err != nil {
				panic(err)
			}
			var decoded config
			if err := yaml.Unmarshal(encoded, &decoded); err != nil {
				panic(err)
			}
			declarations[index].decoded = &decoded
		}
		out = append(out, declarations...)
	}
	return out
}()

func mergeConfig(dst, src map[string]any) {
	for key, value := range src {
		if object, ok := value.(map[string]any); ok {
			nested, ok := dst[key].(map[string]any)
			if !ok {
				nested = map[string]any{}
			}
			mergeConfig(nested, object)
			dst[key] = nested
		} else {
			dst[key] = value
		}
	}
}

// resolveConfig applies exact path prefixes from broad to fine. Documents win
// ties; unmatched defaults are inert, including resources renamed upstream.
func resolveConfig(path []string, levels ...config) (config, error) {
	// Keep the ordinary zero/one-declaration case allocation-free. Generic
	// merging is needed only when multiple hierarchy layers actually contribute.
	var sole *config
	count := 0
	defaultMatch := false
	var singleDefault *config
	defaultCount := 0
	for _, declaration := range configDefaults {
		if len(levels) > 0 && levels[0].providerDefaultsResolved && len(declaration.Path) <= 1 {
			continue
		}
		if len(declaration.Path) <= len(path) && slices.Equal(declaration.Path, path[:len(declaration.Path)]) && len(declaration.Config) > 0 {
			defaultMatch = true
			defaultCount++
			singleDefault = declaration.decoded
		}
	}
	if defaultCount == 1 && singleDefault != nil {
		levelsEmpty := true
		for index := range levels {
			if len(levels[index].raw) > 0 {
				levelsEmpty = false
				break
			}
		}
		if levelsEmpty {
			return *singleDefault, nil
		}
	}
	if !defaultMatch {
		for index := range levels {
			if len(levels[index].raw) > 0 {
				count++
				sole = &levels[index]
			}
		}
		if count == 0 {
			return config{}, nil
		}
		if count == 1 {
			return *sole, nil
		}
	}
	merged := map[string]any{}
	for depth := 0; depth <= len(path); depth++ {
		for _, declaration := range configDefaults {
			if len(levels) > 0 && levels[0].providerDefaultsResolved && len(declaration.Path) <= 1 {
				continue
			}
			if len(declaration.Path) == depth && slices.Equal(declaration.Path, path[:depth]) {
				mergeConfig(merged, declaration.Config)
			}
		}
		if depth > 0 && depth <= len(levels) {
			mergeConfig(merged, levels[depth-1].raw)
		}
	}

	data, err := yaml.Marshal(merged)
	if err != nil {
		return config{}, err
	}
	var resolved config
	if err := yaml.Unmarshal(data, &resolved); err != nil {
		return config{}, fmt.Errorf("config: %w", err)
	}
	return resolved, nil
}

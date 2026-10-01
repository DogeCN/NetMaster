package route

import (
	_ "embed"
	"strings"
)

//go:embed rules_default.txt
var defaultRules string

// LoadDefault 加载内置默认规则表。
func LoadDefault(def Action) (*Router, error) {
	r := New(def)
	if err := r.parse(strings.NewReader(defaultRules)); err != nil {
		return nil, err
	}
	return r, nil
}

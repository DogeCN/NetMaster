// Package config 负责 config.json 的查找与解析（标准库零依赖）。
//
// 配置来源优先级：命令行 flag > 配置文件 > 默认值。实现上，配置文件只用来
// 提供 flag 的默认值 —— flag 总是显式声明，配置值被当作"默认的默认"，所以
// 优先级天然成立，不需要任何合并逻辑。
//
// 可配置项与 serve 的 flag 一一对应（名字去掉 --）：server / password /
// manual / rules。nodes / restore 同样读取 server / password。
//
// 查找顺序：./config.json，然后 %AppData%/netmaster/config.json
// （os.UserConfigDir）。两个位置都不存在不是错误 —— 所有配置也都可以由
// flag 给出。文件存在但损坏是错误：静默忽略一份读不出来的配置，会让人
// 以为它生效了。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Config 是 config.json 的结构。
type Config struct {
	Server   string `json:"server,omitempty"`
	Password string `json:"password,omitempty"`
	Manual   bool   `json:"manual,omitempty"`
	Rules    string `json:"rules,omitempty"`
}

// searchPaths 返回按优先级排列的候选路径。
func searchPaths() []string {
	paths := []string{"config.json"}
	if base, err := os.UserConfigDir(); err == nil && base != "" {
		paths = append(paths, filepath.Join(base, "netmaster", "config.json"))
	}
	return paths
}

// Load 读配置。返回 (配置, 实际加载的文件路径)。
func Load() (Config, string, error) {
	for _, p := range searchPaths() {
		b, err := os.ReadFile(p)
		if err != nil {
			continue // 不存在或读不了：换下一个位置
		}
		var c Config
		if err := json.Unmarshal(b, &c); err != nil {
			return Config{}, p, fmt.Errorf("parse %s: %w", p, err)
		}
		return c, p, nil
	}
	return Config{}, "", nil
}

// template 是首次启动时写出的 config.json 样例。占位符本身是合法 JSON，
// 用户只要替换两个尖括号里的值。
const template = `{
  "server": "<your worker domain, e.g. nm.example.com>",
  "password": "<the PASSWORD you set on the Worker>"
}
`

// WriteTemplate 在当前目录写出配置模板，返回绝对路径。
// 只在"完全没有配置文件"时由 serve 的双击路径调用；任意位置已有配置则不动。
func WriteTemplate() (string, error) {
	if _, found, _ := Load(); found != "" {
		return "", nil
	}
	abs, _ := filepath.Abs("config.json")
	if err := os.WriteFile("config.json", []byte(template), 0o644); err != nil {
		return abs, err
	}
	return abs, nil
}

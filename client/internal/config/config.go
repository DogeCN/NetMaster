// Package config 负责 config.json 的查找与解析（标准库零依赖）。
//
// 配置来源优先级：命令行 flag > 配置文件 > 默认值。实现上，配置文件只用来
// 提供 flag 的默认值 —— flag 总是显式声明，配置值被当作"默认的默认"，所以
// 优先级天然成立，不需要任何合并逻辑。
//
// 可配置项与 serve 的 flag 一一对应（名字去掉 --）：server / password /
// manual / rules / no-ech / tunnels。insecure 只有配置项没有 flag（自我签名
// 证书场景太窄，不值得占一行命令行帮助）。nodes / restore 同样读取 server /
// password。
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
//
// 字段与 serve 的 flag 一一对应，不多一个：曾经有个 LatencyToleranceMs
// （出口优选容差），它描述的"延迟容忍 / 自优化选路"在 v2 里已随 nodepool
// 一起退役，留着只会让用户以为填了有用。
type Config struct {
	Server   string `json:"server,omitempty"`
	Password string `json:"password,omitempty"`
	Manual   bool   `json:"manual,omitempty"`
	Rules    string `json:"rules,omitempty"`
	// Insecure 关闭出口连接的证书校验。缺省（不写）= 校验开启 —— 标准部署下
	// 客户端总是用 server 域名做 SNI，边缘返回的就是该域名的正规证书，校验
	// 应当通过；只有自建网关用自签证书等场景才需要显式设为 true。
	Insecure *bool `json:"insecure,omitempty"`
	// NoECH 关闭对"客户端→Worker"这条链路隐藏 SNI 的 ECH。缺省（不写）= 启用。
	//
	// 为什么需要这个开关：ECH 的兜底原先只挂在 TLS 层（TLS 失败才退普通 TLS）。
	// 但 ECH 会让 TLS 握手成功、而后续的 HTTP 升级落到别的站点，边缘回一个非 101
	// 响应 —— 那时 TLS 层看不出异常，"ECH 失败就退普通 TLS"的兜底救不了，只能靠
	// 每次都退化成重试。留一个能关掉它的开关，线上出现"偶发 bad handshake"时
	// 才有办法做对照实验。
	NoECH *bool `json:"no-ech,omitempty"`
	// Tunnels 是同时保持的代理隧道条数。缺省（不写）= 4。
	//
	// 为什么开放它：条数直接决定免费版的 DO 时长额度花得多快（每条常连隧道都
	// 按 DO 时长计费），而额度上限只能按"别人怎么用"估。有人网络差、4 条会
	// 撞额度，愿意降到 2 换额度；有人只做低频浏览，1 条就够。
	//
	// 只接受 1..8：不写就用默认；写了超范围由调用方报错退出，而不是静默夹到
	// 边界 —— 静默夹取会让人以为自己真的跑在 8 条上。
	Tunnels *int `json:"tunnels,omitempty"`
	// FragOOB 让分片直连的第一片用 TCP 紧急数据（MSG_OOB）发出（tlsfrag.OOB）。
	// 缺省（不写）= 关闭：紧急字节是否进入对端字节流取决于 SO_OOBINLINE，开了的
	// 接收方会拿到坏记录，所以只在确认目标站点"普通分片被拦、OOB 能过"时手动打开。
	FragOOB *bool `json:"frag-oob,omitempty"`
}

// FragOOBValue 返回分片 OOB 开关的生效值。nil 视为 false（关闭）。
func (c Config) FragOOBValue() bool {
	return c.FragOOB != nil && *c.FragOOB
}

// TunnelsValue 返回配置里写的隧道条数；没写（nil）返回 0，由调用方按"用默认"处理。
func (c Config) TunnelsValue() int {
	if c.Tunnels == nil {
		return 0
	}
	return *c.Tunnels
}

// InsecureEnabled 返回证书校验开关的生效值。nil 视为 false（校验开启）。
func (c Config) InsecureEnabled() bool {
	return c.Insecure != nil && *c.Insecure
}

// ECHDisabled 返回 ECH 开关的生效值。nil 视为 false（ECH 启用）。
func (c Config) ECHDisabled() bool {
	return c.NoECH != nil && *c.NoECH
}

// LockPath 返回 serve 单实例锁文件的路径：与实际加载的 config.json 同目录；
// 没有配置文件时退到 UserConfigDir/netmaster，再退到当前目录。
func LockPath(cfgPath string) string {
	if cfgPath != "" {
		return filepath.Join(filepath.Dir(cfgPath), "serve.lock")
	}
	if base, err := os.UserConfigDir(); err == nil && base != "" {
		return filepath.Join(base, "netmaster", "serve.lock")
	}
	return "serve.lock"
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
//
// 故意只写必填的两项：manual / rules 是可选增强，把四个字段全铺开会让
// 第一次打开这个文件的人以为都得填。要用它们照 README 加即可。
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

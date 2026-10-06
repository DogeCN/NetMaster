// Package config 负责全部持久化数据的读写：一份文件存所有东西。
//
// 文件固定在 %AppData%/netmaster/netmaster.json（os.UserConfigDir），包含
// 配置（server/password/开关）与节点优选缓存两块。程序目录不再落任何文件
// ——双击运行的用户不该在 exe 旁边看到一坨随行文件。
//
// 配置来源优先级：命令行 flag > 配置文件 > 默认值。实现上，配置文件只用来
// 提供 flag 的默认值 —— flag 总是显式声明，配置值被当作"默认的默认"，所以
// 优先级天然成立，不需要任何合并逻辑。
//
// 兼容：老版本把 config.json 放在程序目录（./config.json）。首次用新版启动
// 时若 appdata 文件不存在而 ./config.json 存在，LoadFile 会把它原样读回；
// 调用方（serve）随后 Save 一次即完成迁移，老文件留在原地不动。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"netmaster/internal/entry"
)

// Config 是配置块的结构。
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
	// NoFrag 关掉"分片直连"这条降级路径：分流判 proxy 的目标不再赌直连（直接走
	// 隧道），规则直连的目标被拦后也不再补一枪分片（直接改道）。
	//
	// 为什么需要它：直连的成败判据只有传输层（首字节有没有回来），看不见应用层
	// 语义 —— 站点 WAF 按来源 IP 拒绝时回的是合法 403，阶梯会当成"直连成立"，
	// 不但不回退，还把 fragDirect 记 6 小时。2026-10-06 的 arena.ai 就是这种：
	// 本机直连 403（CF-RAY …-SEA）、经香港中继 200（…-HKG）。对这类站点，
	// 唯一的解就是别赌直连。
	NoFrag *bool `json:"no-frag,omitempty"`
}

// NoFragEnabled 返回"关掉分片直连"开关的生效值。nil 视为 false（保留直连赌注）。
func (c Config) NoFragEnabled() bool {
	return c.NoFrag != nil && *c.NoFrag
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

// EntryCache 是节点优选结果的持久化块：上次社区源拉到的候选 IP 与时间。
//
// 优选缓存的意义是启动不等社区源：有缓存时立刻进池，后台再决定要不要刷新
// （不够多或太旧才刷）。缓存里只是"值得试的候选"，不是可用性结论 —— 可用性
// 由池子的拨号失败记忆管。
type EntryCache struct {
	When  time.Time    `json:"when"`
	Nodes []entry.Node `json:"nodes"`
}

// File 是持久化文件的整体结构：配置块 + 缓存块，一个文件存所有需要落盘的
// 数据。Config 内嵌以保持平铺 —— 老版 config.json 里手填的字段原样可读。
type File struct {
	Config
	EntryCache *EntryCache `json:"entry_cache,omitempty"`
}

// appPathOverride 供测试把持久化文件钉进临时目录；非空时 AppPath 直接返回它。
var appPathOverride string

// AppPath 返回持久化文件的路径：%AppData%/netmaster/netmaster.json。
// UserConfigDir 拿不到（极老系统/容器）时退到当前目录，功能不受影响。
func AppPath() string {
	if appPathOverride != "" {
		return appPathOverride
	}
	if base, err := os.UserConfigDir(); err == nil && base != "" {
		return filepath.Join(base, "netmaster", "netmaster.json")
	}
	return "netmaster.json"
}

// legacyPath 是老版本在程序目录留下的配置。只为迁移而读，不再写入。
const legacyPath = "config.json"

// LegacyPath 返回老版本配置的路径，供调用方判断"刚加载的是否需要迁移"。
func LegacyPath() string { return legacyPath }

// LockPath 返回 serve 单实例锁文件的路径：与持久化文件同目录。
func LockPath(_ string) string {
	return filepath.Join(filepath.Dir(AppPath()), "serve.lock")
}

// LoadFile 读持久化文件。查找顺序：appdata 的 netmaster.json → 老的
// ./config.json（只读不写，调用方 Save 一次即完成迁移）。
// 两个位置都不存在不是错误 —— 所有配置也都可以由 flag / 首次交互给出。
// 文件存在但损坏是错误：静默忽略一份读不出来的配置，会让人以为它生效了。
func LoadFile() (File, string, error) {
	for _, p := range []string{AppPath(), legacyPath} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue // 不存在或读不了：换下一个位置
		}
		var f File
		if err := json.Unmarshal(b, &f); err != nil {
			return File{}, p, fmt.Errorf("parse %s: %w", p, err)
		}
		return f, p, nil
	}
	return File{}, "", nil
}

// LoadFileAt 显式读指定路径（测试用；生产路径由 AppPath 决定）。
func LoadFileAt(path string) (File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return File{}, err
	}
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		return File{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return f, nil
}

// Load 只取配置块（nodes/restore 等不关心缓存的调用方用）。
func Load() (Config, string, error) {
	f, p, err := LoadFile()
	return f.Config, p, err
}

// Save 把整份持久化数据写进 appdata 文件（目录不存在则创建）。
// 文件里含口令，权限收窄到属主可读写。
func Save(f File) (string, error) {
	p := AppPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return p, err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return p, err
	}
	if err := os.WriteFile(p, append(b, '\n'), 0o600); err != nil {
		return p, err
	}
	return p, nil
}

// Reset 清除所有持久化数据：appdata 文件、serve 单实例锁，以及老版本留在
// 程序目录的 config.json（若存在）。文件不存在不算错误。
func Reset() []string {
	var removed []string
	for _, p := range []string{AppPath(), LockPath(""), legacyPath} {
		if err := os.Remove(p); err == nil {
			removed = append(removed, p)
		}
	}
	return removed
}

// SaveEntryCache 把节点优选结果落进持久化文件（其余内容原样保留）。
// 由 serve 的后台刷新调用：刷新成功才有必要写，失败静默 —— 缓存是优化不是状态。
func SaveEntryCache(nodes []entry.Node) (string, error) {
	f, _, err := LoadFile()
	if err != nil {
		return AppPath(), err
	}
	f.EntryCache = &EntryCache{When: time.Now(), Nodes: nodes}
	return Save(f)
}

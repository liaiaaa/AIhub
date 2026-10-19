package main

// WorkBuddy 桌面端 5.7.6（2026-10-08 自动升级）起，国内线路的凭据文件
// %LOCALAPPDATA%\CodeBuddyExtension\Data\Public\auth\workbuddy-desktop.info
// 里的敏感字段不再写明文，而是 AES-256-GCM 信封：
//
//	"nickname": {"$wbEncrypted":1,"envelope":"<base64({suite,keyId,nonce,authTag,ciphertext})>"}
//
// 于是 auth.go 的 json.Unmarshal 直接报
// “cannot unmarshal object into Go struct field SessionFile.account.nickname of type string”，
// 表现为控制台「读取凭据失败」。
//
// 本文件负责读时解封、刷新写回时回封，让 hub-go 继续共用桌面端那份**实时**凭据
// （桌面端刷新 token 后 hub-go 立刻就能拿到新的），而不是让用户手工导出一份会过期的明文。
//
// 密钥（逆向 app.asar 与 cli/dist/codebuddy-headless.js 得到）：
//   定制 Electron 原生绑定 workbuddyStorage.loggerGet() 返回构建期密钥 JSON
//   {version, atRestSecretKey(44 字符 canonical base64), atRestDeveloperPublicKey}；
//   字段信封用的 AES key = sha256(atRestSecretKey **字符串**的 UTF-8 字节)
//   （normalizeAtRestKeyPayload），keyId = sha256(key) 前 8 字节的 hex。
//   密钥不落盘（对安装目录 + 数据目录做 sha256 滑窗扫描零命中），只能从运行中的
//   主进程取；因此这里缓存在数据根的 wbatrest.key。一次性取法见 qoder-sandbox\cdp*。
//
// AAD = buildAuthenticatedContextAad(keyId, suite, {framing}, "sym-v1")：
//   "WB-AAD\0" | 0x01 | LP(framingTag) | LP("sym-v1") | u32(suite)
//   | LP(keyId) | framingByte | encodeOptionalUint64(undefined)=0x00 | final=0x00
//   LP(s) = u32(len(utf8(s))) + s；file/field framing 在 sym-v1 下禁止携带
//   sequence/final，所以尾部两字节恒为 0。
//   framingTag/byte：file="WBEF1"/1、field="WBEV1"/2、record="WBER1"/3、stream="WBES1"/4。

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const (
	wbEncMarker   = "$wbEncrypted"
	wbKeyFileName = "wbatrest.key"
)

// wbFieldMeta 记住某个字段原本的信封参数，回封时按原样重建。
type wbFieldMeta struct {
	KeyID   string
	Suite   int
	Framing string
}

// wbEnvelope 是 base64 解开后的信封 JSON。字段顺序与桌面端一致。
type wbEnvelope struct {
	Suite      int    `json:"suite"`
	KeyID      string `json:"keyId"`
	Nonce      string `json:"nonce"`
	AuthTag    string `json:"authTag"`
	Ciphertext string `json:"ciphertext"`
}

type wbCodec struct {
	mu      sync.Mutex
	keys    [][]byte          // 候选密钥，按可用性顺序尝试
	keyIDOf map[string][]byte // keyId -> 实际可用的密钥
	raw     []byte            // 文件里存的密钥字符串（用于派生候选）
	loaded  bool
	loadErr error
}

var wbCodecInstance = &wbCodec{}

func wbKeyPath() string { return dataPath(wbKeyFileName) }

// wbKeyGlob 备用密钥的文件名模式。见 loadKeys 的密钥环说明。
const wbKeyGlob = "wbatrest-*.key"

// loadKeys 读密钥环，把所有可用密钥都收进候选列表。
//
// 为什么必须是「环」而不是单把：桌面端 5.7.6 的 at-rest 密钥**按安装/移植各自生成**
// —— Linux 移植版那个 shim 在找不到 ~/.workbuddy-ai/.at-rest-key 时会
// crypto.randomBytes(32) 现生成一把（见 port-workbuddy-to-linux.sh 生成的
// resources/app/main.js），于是国内凭据和国际实时凭据用的是**两把不同的密钥**
// （keyId 9127dea1… 与 44b936df…）。只留一个 wbatrest.key 的话，换密钥就弄坏另一条线。
//
// 允许两种写法，各自作为候选：
//  1. 已派生的 32 字节 AES key 的 base64（推荐，hub-go 现用）
//  2. 桌面端 loggerGet() 里那串 44 字符 atRestSecretKey 原文
//     （真正的 AES key 需要再对「字符串本身」做一次 sha256）
//
// 主文件 wbatrest.key 缺失不再直接报错：只要有一个备用文件能用就行。
func (c *wbCodec) loadKeys() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loaded {
		return c.loadErr
	}
	c.loaded = true
	c.keyIDOf = map[string][]byte{}

	files := []string{}
	if _, err := os.Stat(wbKeyPath()); err == nil {
		files = append(files, wbKeyPath())
	}
	if alt, err := filepath.Glob(dataPath(wbKeyGlob)); err == nil {
		files = append(files, alt...)
	}
	if len(files) == 0 {
		c.loadErr = fmt.Errorf("WorkBuddy 凭据已被 5.7.6 加密，缺少解密用密钥文件：%s（或 %s；内容取自桌面端主进程，见 workbuddy_atrest.go 顶部说明）", wbKeyPath(), dataPath(wbKeyGlob))
		return c.loadErr
	}

	var problems []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s：%v", filepath.Base(f), err))
			continue
		}
		s := string(bytes.TrimSpace(b))
		raw, derr := base64.StdEncoding.DecodeString(s)
		if derr != nil || len(raw) != 32 {
			problems = append(problems, fmt.Sprintf("%s 格式不对：应为 32 字节密钥的 canonical base64", filepath.Base(f)))
			continue
		}
		c.keys = append(c.keys, raw)
		if derived := sha256.Sum256([]byte(s)); !bytes.Equal(derived[:], raw) {
			c.keys = append(c.keys, derived[:])
		}
	}
	if len(c.keys) == 0 {
		c.loadErr = fmt.Errorf("没有可用的凭据解密密钥（%s）", strings.Join(problems, "；"))
		return c.loadErr
	}
	return nil
}

func wbLP(s string) []byte {
	b := []byte(s)
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, uint32(len(b)))
	return append(out, b...)
}

// wbAad 复刻 sym-v1 的 AAD 拼接。
func wbAad(keyID string, suite int, framing string) []byte {
	tag, marker := "WBEV1", byte(2)
	switch framing {
	case "file":
		tag, marker = "WBEF1", 1
	case "record":
		tag, marker = "WBER1", 3
	case "stream":
		tag, marker = "WBES1", 4
	}
	aad := []byte("WB-AAD\x00")
	aad = append(aad, 0x01)
	aad = append(aad, wbLP(tag)...)
	aad = append(aad, wbLP("sym-v1")...)
	var u32 [4]byte
	binary.BigEndian.PutUint32(u32[:], uint32(suite))
	aad = append(aad, u32[:]...)
	aad = append(aad, wbLP(keyID)...)
	aad = append(aad, marker)
	aad = append(aad, 0x00, 0x00) // 无 sequence、无 final
	return aad
}

func wbKeyIDOf(key []byte) string {
	sum := sha256.Sum256(key)
	return fmt.Sprintf("%x", sum[:8])
}

// open 解一个信封，返回明文。
func (c *wbCodec) open(framing, envB64 string) (string, error) {
	if err := c.loadKeys(); err != nil {
		return "", err
	}
	eb, err := base64.StdEncoding.DecodeString(envB64)
	if err != nil {
		return "", fmt.Errorf("信封 base64 非法：%v", err)
	}
	var env wbEnvelope
	if err := json.Unmarshal(eb, &env); err != nil {
		return "", fmt.Errorf("信封 JSON 解析失败：%v", err)
	}
	if env.Suite != 1 {
		return "", fmt.Errorf("不支持的加密套件 %d（只支持 suite=1 AES-256-GCM）", env.Suite)
	}
	nonce, err := base64.StdEncoding.DecodeString(env.Nonce)
	if err != nil {
		return "", err
	}
	tag, err := base64.StdEncoding.DecodeString(env.AuthTag)
	if err != nil {
		return "", err
	}
	ct, err := base64.StdEncoding.DecodeString(env.Ciphertext)
	if err != nil {
		return "", err
	}
	aad := wbAad(env.KeyID, env.Suite, framing)
	sealed := append(append([]byte{}, ct...), tag...)

	c.mu.Lock()
	defer c.mu.Unlock()
	if k := c.keyIDOf[env.KeyID]; k != nil {
		if pt, err := wbGCMOpen(k, nonce, sealed, aad); err == nil {
			return string(pt), nil
		}
	}
	var last error
	for _, k := range c.keys {
		if wbKeyIDOf(k) != env.KeyID {
			continue
		}
		pt, err := wbGCMOpen(k, nonce, sealed, aad)
		if err != nil {
			last = err
			continue
		}
		c.keyIDOf[env.KeyID] = k
		return string(pt), nil
	}
	if last == nil {
		last = fmt.Errorf("本地密钥与 keyId=%s 不匹配", env.KeyID)
	}
	return "", fmt.Errorf("凭据解密失败（keyId=%s，桌面端可能已升级到新密钥）：%v", env.KeyID, last)
}

func wbGCMOpen(key, nonce, sealed, aad []byte) ([]byte, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	if len(nonce) != g.NonceSize() {
		return nil, fmt.Errorf("nonce 长度 %d 不对", len(nonce))
	}
	return g.Open(nil, nonce, sealed, aad)
}

// seal 把刷新得到的明文重新封成桌面端格式。
func (c *wbCodec) seal(m wbFieldMeta, plaintext string) (map[string]any, error) {
	if err := c.loadKeys(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	k := c.keyIDOf[m.KeyID]
	if k == nil {
		for _, cand := range c.keys {
			if wbKeyIDOf(cand) == m.KeyID {
				k = cand
				break
			}
		}
	}
	if k == nil {
		return nil, fmt.Errorf("没有可用于 keyId=%s 的密钥", m.KeyID)
	}
	blk, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := g.Seal(nil, nonce, []byte(plaintext), wbAad(m.KeyID, m.Suite, m.Framing))
	env := wbEnvelope{
		Suite:      m.Suite,
		KeyID:      m.KeyID,
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		AuthTag:    base64.StdEncoding.EncodeToString(out[len(out)-g.Overhead():]),
		Ciphertext: base64.StdEncoding.EncodeToString(out[:len(out)-g.Overhead()]),
	}
	eb, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	c.keyIDOf[m.KeyID] = k
	return map[string]any{wbEncMarker: float64(1), "envelope": base64.StdEncoding.EncodeToString(eb)}, nil
}

// asEnvelope 判断一个值是不是信封对象，并取出 envelope 串。
// UseNumber 解析出来的数字是 json.Number，结构体解析的是 float64，都要认。
func asEnvelope(v any) (envB64 string, ok bool) {
	m, isMap := v.(map[string]any)
	if !isMap {
		return "", false
	}
	switch n := m[wbEncMarker].(type) {
	case json.Number:
		if n.String() != "1" {
			return "", false
		}
	case float64:
		if n != 1 {
			return "", false
		}
	default:
		return "", false
	}
	s, isStr := m["envelope"].(string)
	if !isStr || s == "" {
		return "", false
	}
	return s, true
}

func wbMetaOf(envB64 string) wbFieldMeta {
	m := wbFieldMeta{Suite: 1, Framing: "field"}
	eb, err := base64.StdEncoding.DecodeString(envB64)
	if err != nil {
		return m
	}
	var env wbEnvelope
	if json.Unmarshal(eb, &env) != nil {
		return m
	}
	if env.KeyID != "" {
		m.KeyID = env.KeyID
	}
	if env.Suite != 0 {
		m.Suite = env.Suite
	}
	return m
}

// wbUnsealAny 递归把信封换成明文，返回新值；meta 记录被换掉的位置与原始参数。
func wbUnsealAny(v any, meta map[string]wbFieldMeta, path string) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, sub := range t {
			p := path + "." + k
			if envB64, isEnv := asEnvelope(sub); isEnv {
				pt, err := wbCodecInstance.open("field", envB64)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", p, err)
				}
				if meta != nil {
					meta[p] = wbMetaOf(envB64)
				}
				out[k] = pt
				continue
			}
			nv, err := wbUnsealAny(sub, meta, p)
			if err != nil {
				return nil, err
			}
			out[k] = nv
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, sub := range t {
			p := path + "." + strconv.Itoa(i)
			nv, err := wbUnsealAny(sub, meta, p)
			if err != nil {
				return nil, err
			}
			out[i] = nv
		}
		return out, nil
	}
	return v, nil
}

// wbUnsealBytes 把 auth 文件里的信封全部解成明文，返回可直接喂给
// json.Unmarshal 的字节 + 原始信封位置表。没有信封时原样返回（国际线路快照）。
func wbUnsealBytes(b []byte) ([]byte, map[string]wbFieldMeta, error) {
	if !bytes.Contains(b, []byte(wbEncMarker)) {
		return b, nil, nil
	}
	var tree any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&tree); err != nil {
		return nil, nil, fmt.Errorf("auth 文件不是合法 JSON：%v", err)
	}
	meta := map[string]wbFieldMeta{}
	plain, err := wbUnsealAny(tree, meta, "")
	if err != nil {
		return nil, nil, err
	}
	out, err := json.Marshal(plain)
	if err != nil {
		return nil, nil, err
	}
	return out, meta, nil
}

// wbResealBytes 把回写内容里被解封过的字段重新加密，保持桌面端能读的格式。
// 只处理 meta 记录过、且当前值是明文字符串的位置；已经是信封的位置不动。
func wbResealBytes(b []byte, meta map[string]wbFieldMeta) ([]byte, error) {
	if len(meta) == 0 {
		return b, nil
	}
	var tree any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&tree); err != nil {
		return b, nil
	}
	n, err := wbResealAny(tree, meta, "")
	if err != nil {
		return nil, err
	}
	out, err := json.MarshalIndent(n, "", "  ")
	if err != nil {
		return nil, err
	}
	return out, nil
}

func wbResealAny(v any, meta map[string]wbFieldMeta, path string) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		for k, sub := range t {
			p := path + "." + k
			if m, isEnv := meta[p]; isEnv {
				if s, isStr := sub.(string); isStr && s != "" {
					sealed, err := wbCodecInstance.seal(m, s)
					if err != nil {
						return nil, fmt.Errorf("%s: %w", p, err)
					}
					t[k] = sealed
				}
				continue
			}
			nv, err := wbResealAny(sub, meta, p)
			if err != nil {
				return nil, err
			}
			t[k] = nv
		}
	case []any:
		for i, sub := range t {
			p := path + "." + strconv.Itoa(i)
			nv, err := wbResealAny(sub, meta, p)
			if err != nil {
				return nil, err
			}
			t[i] = nv
		}
	}
	return v, nil
}

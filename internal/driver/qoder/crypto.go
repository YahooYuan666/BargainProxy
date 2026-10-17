package qoder

// Qoder 网关 COSY 协议的加密/签名层（移植自 Qoder-2API-Go auth 包）：
// 自定义 Base64 变体编码、MD5 wire 签名、RSA+AES 混合加密构造会话 bearer。

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	cmd5 "crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ---- 自定义 Base64 变体（Qoder 网关 Encode 层）----

const customAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
const stdAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

var s2c, c2s [128]byte
var s2cOK, c2sOK [128]bool

func init() {
	for i := 0; i < 64; i++ {
		s2c[stdAlphabet[i]] = customAlphabet[i]
		s2cOK[stdAlphabet[i]] = true
		c2s[customAlphabet[i]] = stdAlphabet[i]
		c2sOK[customAlphabet[i]] = true
	}
	s2c['='] = '$'
	s2cOK['='] = true
	c2s['$'] = '='
	c2sOK['$'] = true
}

// gatewayEncode 标准明文 → 网关自定义编码串。
func gatewayEncode(plaintext []byte) (string, error) {
	std := base64.StdEncoding.EncodeToString(plaintext)
	n := len(std)
	a := n / 3
	rearranged := make([]byte, n)
	copy(rearranged[:a], std[n-a:])
	copy(rearranged[a:n-a], std[a:n-a])
	copy(rearranged[n-a:], std[:a])
	out := make([]byte, n)
	for i, ch := range rearranged {
		if ch >= 128 || !s2cOK[ch] {
			return "", fmt.Errorf("char out of alphabet: %q", ch)
		}
		out[i] = s2c[ch]
	}
	return string(out), nil
}

// ---- MD5 wire 签名 ----

const appCode = "cosy"
const defaultSecret = "d2FyLCB3YXIgbmV2ZXIgY2hhbmdlcw=="

func md5Hex(s string) string {
	sum := cmd5.Sum([]byte(s))
	return fmt.Sprintf("%x", sum)
}

// signDate 请求签名用的 UTC HTTP 日期。
func signDate() string { return time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT") }

func signDateMD5(date string) string {
	return md5Hex(appCode + "&" + defaultSecret + "&" + date)
}

// ---- RSA + AES 会话构造 ----

const serverPubKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

var rsaPubKey *rsa.PublicKey

func init() {
	block, _ := pem.Decode([]byte(serverPubKeyPEM))
	if block == nil {
		panic("qoder: parse server public key PEM failed")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		panic(fmt.Sprintf("qoder: parse server public key: %v", err))
	}
	rsaPubKey = pub.(*rsa.PublicKey)
}

func rsaEncrypt(tempKey []byte) ([]byte, error) {
	return rsa.EncryptPKCS1v15(rand.Reader, rsaPubKey, tempKey)
}

func aesEncrypt(plain, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	padLen := 16 - len(plain)%16
	padded := make([]byte, len(plain)+padLen)
	copy(padded, plain)
	for i := len(plain); i < len(padded); i++ {
		padded[i] = byte(padLen)
	}
	encrypted := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, key[:16]).CryptBlocks(encrypted, padded)
	return encrypted, nil
}

// authIdentity 签入 bearer 载荷的账号身份。
type authIdentity struct {
	Name               string `json:"name"`
	Aid                string `json:"aid"`
	UID                string `json:"uid"`
	YxUID              string `json:"yx_uid"`
	OrganizationID     string `json:"organization_id"`
	OrganizationName   string `json:"organization_name"`
	UserType           string `json:"user_type"`
	SecurityOauthToken string `json:"security_oauth_token"`
	RefreshToken       string `json:"refresh_token"`
}

// sessionContext 一次会话的加密上下文（bearer 由它派生）。
type sessionContext struct {
	cosyKey string
	info    string
	ident   authIdentity
	machineID,
	machineToken,
	machineType string
}

// newSession 由身份构造加密会话（tempKey 随机，每次 jobToken 后重建）。
func newSession(ident authIdentity, machineID, machineToken, machineType string) (*sessionContext, error) {
	tempKey := []byte(newUUID()[:16])
	encrypted, err := rsaEncrypt(tempKey)
	if err != nil {
		return nil, err
	}
	cosyKey := base64.StdEncoding.EncodeToString(encrypted)
	plain, err := marshalNoEscape(ident)
	if err != nil {
		return nil, err
	}
	aesEnc, err := aesEncrypt(plain, tempKey)
	if err != nil {
		return nil, err
	}
	return &sessionContext{
		cosyKey:      cosyKey,
		info:         base64.StdEncoding.EncodeToString(aesEnc),
		ident:        ident,
		machineID:    machineID,
		machineToken: machineToken,
		machineType:  machineType,
	}, nil
}

// cosyPayload bearer 的 COSY 头部载荷。
type cosyPayload struct {
	CosyVersion string `json:"cosyVersion"`
	IdeVersion  string `json:"ideVersion"`
	Info        string `json:"info"`
	RequestID   string `json:"requestId"`
	Version     string `json:"version"`
}

func (s *sessionContext) buildBearer(dateUnix int64, body, pathSig string) string {
	payload := cosyPayload{
		CosyVersion: "0.1.43",
		Info:        s.info,
		RequestID:   newUUID(),
		Version:     "v1",
	}
	raw, err := marshalNoEscape(payload)
	if err != nil {
		return ""
	}
	payloadB64 := base64.StdEncoding.EncodeToString(raw)
	sig := md5Hex(payloadB64 + "\n" + s.cosyKey + "\n" + strconv.FormatInt(dateUnix, 10) + "\n" + body + "\n" + pathSig)
	return "Bearer COSY." + payloadB64 + "." + sig
}

// sigPath 签名路径：去 /algo 前缀。
func sigPath(fullURL string) string {
	u, err := url.Parse(fullURL)
	if err != nil {
		return fullURL
	}
	path := u.Path
	if strings.HasPrefix(path, "/algo") {
		path = path[len("/algo"):]
	}
	return path
}

// ---- 上游端点 ----

const (
	authBase  = "https://gateway.qoder.com.cn"
	openBase  = "https://openapi.qoder.com.cn"
	chatPath  = "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
	modelPath = "/algo/api/v2/model/list?Encode=1"
	jobPath   = "/algo/api/v3/user/jobToken?Encode=1"
	statPath  = "/algo/api/v3/user/status?Encode=1"
	quotaPath = "/api/v2/quota/usage"
)

// ---- 杂项 ----

// marshalNoEscape 紧凑 JSON、不转义 HTML（与网关序列化语义对齐）。
func marshalNoEscape(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case string:
		i, _ := strconv.ParseInt(n, 10, 64)
		return i
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}

func toInt(v any) int { return int(toInt64(v)) }

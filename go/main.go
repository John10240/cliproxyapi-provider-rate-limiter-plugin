package main

/*
#include <stdint.h>
#include <stdlib.h>
typedef struct { void* ptr; size_t len; } cliproxy_buffer;
typedef struct { uint32_t abi_version; void* host_ctx; void* call; void* free_buffer; } cliproxy_host_api;
typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);
typedef struct { uint32_t abi_version; cliproxy_plugin_call_fn call; cliproxy_plugin_free_fn free_buffer; cliproxy_plugin_shutdown_fn shutdown; } cliproxy_plugin_api;
extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

type pluginConfig struct {
	DefaultRPM int            `yaml:"default_rpm" json:"default_rpm"`
	Providers  map[string]int `yaml:"providers" json:"providers"`
	Auths      map[string]int `yaml:"auths" json:"auths"`
}
type window struct {
	mu   sync.Mutex
	hits []time.Time
}

func (w *window) allow(now time.Time, rpm int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	cutoff := now.Add(-time.Minute)
	keep := w.hits[:0]
	for _, t := range w.hits {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	w.hits = keep
	if rpm <= 0 {
		return true
	}
	if len(w.hits) >= rpm {
		return false
	}
	w.hits = append(w.hits, now)
	return true
}

var currentConfig atomic.Value
var windows sync.Map

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}
type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Retryable  bool   `json:"retryable,omitempty"`
}
type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}
type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}
type registrationCapability struct {
	Scheduler     bool `json:"scheduler"`
	ManagementAPI bool `json:"management_api"`
}

type managementRegistration struct {
	Resources []resourceRoute   `json:"resources,omitempty"`
	Routes    []managementRoute `json:"routes,omitempty"`
}

type resourceRoute struct {
	Path        string `json:"path"`
	Menu        string `json:"menu"`
	Description string `json:"description"`
}

type managementRoute struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

type managementRequest struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   []byte `json:"body"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(_ *C.cliproxy_host_api, p *C.cliproxy_plugin_api) C.int {
	if p == nil {
		return 1
	}
	p.abi_version = C.uint32_t(pluginabi.ABIVersion)
	p.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	p.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	p.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, n C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var b []byte
	if request != nil && n > 0 {
		b = C.GoBytes(unsafe.Pointer(request), C.int(n))
	}
	out, err := handleMethod(C.GoString(method), b)
	if err != nil {
		writeResponse(response, errorEnvelope("plugin_error", err.Error()))
		return 1
	}
	writeResponse(response, out)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}
func handleMethod(method string, raw []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := configure(raw); err != nil {
			return nil, err
		}
		return okEnvelope(registrationData())
	case pluginabi.MethodSchedulerPick:
		return pick(raw)
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration{
			Resources: []resourceRoute{{Path: "/menu", Menu: "Provider Rate Limiter", Description: "View and manage Provider/AuthID rate limits."}},
			Routes:    []managementRoute{{Method: "GET", Path: "/settings"}, {Method: "PUT", Path: "/settings"}},
		})
	case pluginabi.MethodManagementHandle:
		return handleManagement(raw)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}
func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return err
		}
	}
	cfg := pluginConfig{Providers: map[string]int{}, Auths: map[string]int{}}
	if len(req.ConfigYAML) > 0 {
		if err := yaml.Unmarshal(req.ConfigYAML, &cfg); err != nil {
			return err
		}
	}
	return setConfig(cfg)
}

func normalizeLimits(input map[string]int, lowerKeys bool) (map[string]int, error) {
	result := make(map[string]int, len(input))
	for rawKey, limit := range input {
		key := strings.TrimSpace(rawKey)
		if lowerKeys {
			key = strings.ToLower(key)
		}
		if key == "" {
			return nil, fmt.Errorf("empty key")
		}
		if limit < 0 {
			return nil, fmt.Errorf("%q has negative limit %d", key, limit)
		}
		if previous, exists := result[key]; exists && previous != limit {
			return nil, fmt.Errorf("duplicate key %q with conflicting limits", key)
		}
		result[key] = limit
	}
	return result, nil
}
func loaded() pluginConfig {
	if v := currentConfig.Load(); v != nil {
		return v.(pluginConfig)
	}
	return pluginConfig{Providers: map[string]int{}, Auths: map[string]int{}}
}

func handleManagement(raw []byte) ([]byte, error) {
	var req managementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	path := strings.TrimRight(strings.TrimSpace(req.Path), "/")
	if strings.HasSuffix(path, "/menu") {
		return okEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}}, Body: []byte(menuHTML())})
	}
	if !strings.HasSuffix(path, "/settings") {
		return okEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusNotFound, Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(`{"error":"not_found"}`)})
	}
	switch strings.ToUpper(strings.TrimSpace(req.Method)) {
	case http.MethodGet:
		return okEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: jsonHeaders(), Body: mustJSON(loaded())})
	case http.MethodPut:
		var cfg pluginConfig
		if err := json.Unmarshal(req.Body, &cfg); err != nil {
			return okEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusBadRequest, Headers: jsonHeaders(), Body: []byte(`{"error":"invalid_json"}`)})
		}
		if err := setConfig(cfg); err != nil {
			return okEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusBadRequest, Headers: jsonHeaders(), Body: mustJSON(map[string]string{"error": err.Error()})})
		}
		return okEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: jsonHeaders(), Body: mustJSON(loaded())})
	default:
		return okEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusMethodNotAllowed, Headers: jsonHeaders(), Body: []byte(`{"error":"method_not_allowed"}`)})
	}
}

func jsonHeaders() http.Header {
	return http.Header{"Content-Type": []string{"application/json; charset=utf-8"}, "Cache-Control": []string{"no-store"}}
}

func setConfig(cfg pluginConfig) error {
	if cfg.DefaultRPM < 0 {
		return fmt.Errorf("default_rpm must be >= 0")
	}
	providers, err := normalizeLimits(cfg.Providers, true)
	if err != nil {
		return fmt.Errorf("providers: %w", err)
	}
	auths, err := normalizeLimits(cfg.Auths, false)
	if err != nil {
		return fmt.Errorf("auths: %w", err)
	}
	cfg.Providers, cfg.Auths = providers, auths
	currentConfig.Store(cfg)
	return nil
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func menuHTML() string {
	return `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Provider Rate Limiter</title>
<style>
:root{color-scheme:light;--line:#d9e0e8;--muted:#64748b;--blue:#2563eb;--bg:#f8fafc}*{box-sizing:border-box}body{font:14px system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;margin:0;padding:28px;max-width:1180px;color:#243447;background:var(--bg)}h1{margin:0 0 6px;font-size:24px}h2{font-size:17px;margin:0 0 14px}.hint,.subtle{color:var(--muted)}.toolbar,.card{background:#fff;border:1px solid var(--line);border-radius:10px;padding:18px;margin:16px 0}.toolbar{display:flex;gap:10px;align-items:end;flex-wrap:wrap}.toolbar .key{flex:1 1 320px}.toolbar label,.field label{display:block;color:#334155;font-weight:600;margin-bottom:6px}input,select{width:100%;padding:9px 10px;border:1px solid #cbd5e1;border-radius:7px;background:#fff;color:#243447}input[type=number]{max-width:180px}button{padding:9px 15px;border:0;border-radius:7px;background:var(--blue);color:#fff;cursor:pointer;font-weight:600}button.secondary{background:#475569}button:disabled{opacity:.6;cursor:default}.actions{display:flex;gap:10px;align-items:center;flex-wrap:wrap}.status{margin-top:12px;padding:10px;border-radius:7px;background:#f1f5f9;white-space:pre-wrap}.status.error{background:#fef2f2;color:#991b1b}.status.ok{background:#ecfdf5;color:#166534}.field{margin-bottom:8px}.table-wrap{overflow:auto;border:1px solid var(--line);border-radius:8px}table{width:100%;border-collapse:collapse;min-width:650px}th,td{text-align:left;padding:10px 12px;border-bottom:1px solid #e5eaf0;vertical-align:middle}th{font-size:12px;text-transform:uppercase;letter-spacing:.02em;color:var(--muted);background:#f8fafc}tr:last-child td{border-bottom:0}.account-name{font-weight:600}.account-id{font:12px ui-monospace,SFMono-Regular,Menlo,monospace;color:var(--muted);word-break:break-all}.badge{display:inline-block;padding:3px 7px;border-radius:999px;background:#e2e8f0;color:#475569;font-size:12px}.badge.disabled{background:#fee2e2;color:#991b1b}.limit-input{max-width:130px}.inherit{color:var(--muted);font-size:12px}.section-head{display:flex;justify-content:space-between;gap:12px;align-items:end;flex-wrap:wrap;margin-bottom:12px}.filters{display:flex;gap:8px;flex-wrap:wrap}.filters input{min-width:220px}.empty{padding:22px;text-align:center;color:var(--muted)}footer{position:sticky;bottom:0;display:flex;justify-content:flex-end;gap:10px;padding:14px 0 0;background:linear-gradient(transparent,var(--bg) 28%)}
</style></head><body>
<h1>Provider Rate Limiter</h1><p class="hint">按账号限制请求速率。账号限制覆盖 Provider 限制，Provider 限制覆盖全局默认值。/ Per-auth sliding-window limits: AuthID overrides Provider, which overrides the default.</p>
<section class="toolbar"><div class="key"><label>CPA 管理密钥 / Management key</label><input id="key" type="password" autocomplete="off" placeholder="仅保存在当前页面 / kept only in this page"></div><div class="actions"><button class="secondary" onclick="loadAll()">读取账号和配置 / Load</button></div></section>
<section class="card"><h2>全局默认 / Global default</h2><div class="field"><label for="default">默认 RPM / Default RPM</label><input id="default" type="number" min="0" step="1" value="0"><div class="subtle">0 表示该层级不限流。/ 0 disables the limit at this level.</div></div></section>
<section class="card"><div class="section-head"><div><h2>Provider 默认限制 / Provider limits</h2><div class="subtle">限制应用到该 Provider 的每个账号，而不是所有账号合计。/ Applied independently to each account.</div></div></div><div class="table-wrap"><table><thead><tr><th>Provider</th><th>账号数 / Accounts</th><th>RPM 覆盖 / RPM override</th></tr></thead><tbody id="providerRows"><tr><td colspan="3" class="empty">请先读取账号 / Load accounts first</td></tr></tbody></table></div></section>
<section class="card"><div class="section-head"><div><h2>账号限制 / Account limits</h2><div class="subtle">每行对应 CPA 的真实 AuthID；留空表示继承 Provider 或全局值。/ Each row is a real CPA AuthID; blank inherits.</div></div><div class="filters"><input id="accountFilter" placeholder="搜索账号 / Search account" oninput="renderAccounts()"><select id="providerFilter" onchange="renderAccounts()"><option value="">全部 Provider / All providers</option></select></div></div><div class="table-wrap"><table><thead><tr><th>账号 / Account</th><th>Provider</th><th>状态 / Status</th><th>RPM 覆盖 / RPM override</th></tr></thead><tbody id="accountRows"><tr><td colspan="4" class="empty">请先读取账号 / Load accounts first</td></tr></tbody></table></div></section>
<div id="status" class="status">请输入管理密钥，然后点击“读取账号和配置”。/ Enter the management key, then click Load.</div><footer><button class="secondary" onclick="loadAll()">重新读取 / Reload</button><button onclick="saveAll()">保存运行时配置 / Save runtime settings</button></footer>
<script>
const endpoint='/v0/management/plugins/provider-rate-limiter/settings';const accountsEndpoint='/v0/management/auth-files';const status=document.getElementById('status');const state={accounts:[],providers:{},auths:{},defaultRPM:0};
function headers(){return {'Content-Type':'application/json','X-Management-Key':document.getElementById('key').value.trim()};}
function esc(value){return String(value??'').replace(/[&<>\"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','\"':'&quot;',"'":'&#39;'}[c]));}
function setStatus(message,kind=''){status.className='status '+kind;status.textContent=message;}
function limitValue(value,required=false){const text=String(value??'').trim();if(!text&&!required)return null;const n=Number(text);if(!Number.isInteger(n)||n<0)throw Error('RPM 必须是大于等于 0 的整数 / RPM must be a non-negative integer');return n;}
async function getJSON(url,options={}){const r=await fetch(url,{...options,headers:{...headers(),...(options.headers||{})}});const text=await r.text();let data={};try{data=text?JSON.parse(text):{};}catch{data={error:text||r.statusText};}if(!r.ok)throw Error(data.error||data.message||('HTTP '+r.status));return data;}
function normalizeAccounts(files){return (Array.isArray(files)?files:[]).map(file=>{const id=String(file.id||file.name||'').trim();return{id,label:String(file.label||file.email||file.account||id).trim(),provider:String(file.provider||file.type||'unknown').trim().toLowerCase(),disabled:Boolean(file.disabled),status:String(file.status||'').trim()};}).filter(item=>item.id);}
function providerNames(){const names=new Set(state.accounts.map(item=>item.provider).filter(Boolean));Object.keys(state.providers).forEach(name=>names.add(name));return Array.from(names).sort();}
function renderProviders(){const rows=document.getElementById('providerRows');const names=providerNames();if(!names.length){rows.innerHTML='<tr><td colspan="3" class="empty">没有发现 Provider / No providers found</td>';return;}rows.innerHTML=names.map(provider=>{const count=state.accounts.filter(item=>item.provider===provider).length;const value=Object.prototype.hasOwnProperty.call(state.providers,provider)?state.providers[provider]:'';return '<tr><td><strong>'+esc(provider)+'</strong></td><td>'+count+'</td><td><input class="limit-input provider-limit" data-provider="'+esc(provider)+'" type="number" min="0" step="1" value="'+esc(value)+'" placeholder="继承全局 / inherit"></td></tr>';}).join('');}
function renderProviderFilter(){const select=document.getElementById('providerFilter');const current=select.value;select.innerHTML='<option value="">全部 Provider / All providers</option>'+providerNames().map(name=>'<option value="'+esc(name)+'">'+esc(name)+'</option>').join('');if(providerNames().includes(current))select.value=current;}
function renderAccounts(){const rows=document.getElementById('accountRows');const query=document.getElementById('accountFilter').value.trim().toLowerCase();const provider=document.getElementById('providerFilter').value;const accounts=state.accounts.filter(item=>(!query||[item.id,item.label,item.provider].join(' ').toLowerCase().includes(query))&&(!provider||item.provider===provider));if(!accounts.length){rows.innerHTML='<tr><td colspan="4" class="empty">没有匹配账号 / No matching accounts</td></tr>';return;}rows.innerHTML=accounts.map(item=>{const value=Object.prototype.hasOwnProperty.call(state.auths,item.id)?state.auths[item.id]:'';const badge=item.disabled?'<span class="badge disabled">已禁用 / disabled</span>':'<span class="badge">'+esc(item.status||'ready')+'</span>';return '<tr><td><div class="account-name">'+esc(item.label)+'</div><div class="account-id">'+esc(item.id)+'</div></td><td>'+esc(item.provider)+'</td><td>'+badge+'</td><td><input class="limit-input auth-limit" data-auth-id="'+esc(item.id)+'" type="number" min="0" step="1" value="'+esc(value)+'" placeholder="继承 / inherit"></td></tr>';}).join('');}
function render(){document.getElementById('default').value=state.defaultRPM;renderProviderFilter();renderProviders();renderAccounts();}
async function loadAll(){if(!document.getElementById('key').value.trim()){setStatus('请输入 CPA 管理密钥 / Enter the CPA management key.','error');return;}setStatus('正在读取账号和配置 / Loading accounts and settings...');try{const [settings,auths]=await Promise.all([getJSON(endpoint),getJSON(accountsEndpoint)]);state.defaultRPM=Number(settings.default_rpm||0);state.providers={...(settings.providers||{})};state.auths={...(settings.auths||{})};state.accounts=normalizeAccounts(auths.files);render();setStatus('已读取 '+state.accounts.length+' 个账号。/ Loaded '+state.accounts.length+' accounts.','ok');}catch(error){setStatus('读取失败 / Load failed: '+error.message,'error');}}
function collectMap(selector,attribute,previous){const result={...(previous||{})};document.querySelectorAll(selector).forEach(input=>{const key=input.getAttribute(attribute);const value=limitValue(input.value);if(value===null)delete result[key];else result[key]=value;});return result;}
async function saveAll(){if(!document.getElementById('key').value.trim()){setStatus('请输入 CPA 管理密钥 / Enter the CPA management key.','error');return;}try{const body={default_rpm:limitValue(document.getElementById('default').value,true),providers:collectMap('.provider-limit','data-provider',state.providers),auths:collectMap('.auth-limit','data-auth-id',state.auths)};const saved=await getJSON(endpoint,{method:'PUT',body:JSON.stringify(body)});state.defaultRPM=Number(saved.default_rpm||0);state.providers={...(saved.providers||{})};state.auths={...(saved.auths||{})};render();setStatus('已保存到运行中的插件。重启 CPA 前请把相同配置写入 config.yaml。/ Saved to the running plugin. Persist the same values in config.yaml before restarting CPA.','ok');}catch(error){setStatus('保存失败 / Save failed: '+error.message,'error');}}
</script></body></html>`
}
func limitFor(cfg pluginConfig, c pluginapi.SchedulerAuthCandidate) int {
	if n, ok := cfg.Auths[c.ID]; ok {
		return n
	}
	if n, ok := cfg.Providers[strings.ToLower(c.Provider)]; ok {
		return n
	}
	return cfg.DefaultRPM
}
func pick(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	cfg := loaded()
	now := time.Now()
	for _, c := range req.Candidates {
		if strings.TrimSpace(c.ID) == "" {
			continue
		}
		key := c.ID
		v, _ := windows.LoadOrStore(key, &window{})
		if v.(*window).allow(now, limitFor(cfg, c)) {
			return okEnvelope(pluginapi.SchedulerPickResponse{AuthID: c.ID, Handled: true})
		}
	}
	return errorEnvelopeWithStatus("provider_rate_limit_exceeded", fmt.Sprintf("all candidates for provider %q are over the configured rate limit", req.Provider), http.StatusTooManyRequests), nil
}
func registrationData() registration {
	return registration{SchemaVersion: pluginabi.SchemaVersion, Metadata: pluginapi.Metadata{Name: "provider-rate-limiter", Version: "0.4.0", Author: "lsmallice", GitHubRepository: "https://github.com/lsmallice/cliproxyapi-provider-rate-limiter-plugin", ConfigFields: []pluginapi.ConfigField{{Name: "default_rpm", Type: pluginapi.ConfigFieldTypeInteger, Description: "Default RPM applied independently to every candidate."}, {Name: "providers", Type: pluginapi.ConfigFieldTypeObject, Description: "Provider name to RPM override map."}, {Name: "auths", Type: pluginapi.ConfigFieldTypeObject, Description: "AuthID to RPM override map; overrides provider and default."}}}, Capabilities: registrationCapability{Scheduler: true, ManagementAPI: true}}
}
func okEnvelope(v any) ([]byte, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return nil, e
	}
	return json.Marshal(envelope{OK: true, Result: b})
}
func errorEnvelope(code, msg string) []byte {
	return errorEnvelopeWithStatus(code, msg, 0)
}

func errorEnvelopeWithStatus(code, msg string, status int) []byte {
	b, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: msg, HTTPStatus: status, Retryable: status == http.StatusTooManyRequests}})
	return b
}
func writeResponse(r *C.cliproxy_buffer, b []byte) {
	if r == nil || len(b) == 0 {
		return
	}
	p := C.CBytes(b)
	if p == nil {
		return
	}
	r.ptr = p
	r.len = C.size_t(len(b))
}

// Package quack 是 DuckDB(quack) 远程实例的极简客户端，基于官方驱动
// github.com/duckdb/duckdb-go/v2（内嵌 DuckDB 2.x 引擎，需 CGO）。
//
// 为什么不用 duckcall：Quack wire 协议按大版本锁定（1.5.5 ↔ 2.0 双向不兼容），
// duckcall 上游最新版仍只实现 wire v1（`versions.go: quackVersionMin/Max = 1`），
// 没有可用的 2.0 版本。官方 Go 驱动随 DuckDB 发版，是 2.0 的可行通道。
//
// 通道选择（2026-10-26 实测，DuckDB 2.0.0 服务端）：
//   - `ATTACH 'quack:host:port' AS r (...)` 后，**直接引用 r.<表名> 会被客户端目录
//     缓存挡住**：ATTACH 之后新建的表在本地 catalog 里看不到（需 CALL quack_clear_cache()）。
//   - `SELECT * FROM r.query('<SQL>')` 把 SQL 原样发到服务端执行，不受本地目录缓存影响，
//     且支持多语句（`SET ...; INSERT ...`）、DDL、DML、SELECT，类型往返正确
//     （TIMESTAMPTZ→time.Time、BLOB→[]byte、BIGINT→int64）。
//     因此统一走 r.query() 透传，与旧 duckcall 的「发一条 SQL、收结果行」语义一一对应。
//
// 会话语义：r.query() 每次在服务端新建会话，故 SET 只对**同一次调用内**的后续语句生效。
// 需要跨语句保持会话设置时，把它们拼进同一条 SQL（backfill 已是这种做法）。
package quack

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"

	_ "github.com/duckdb/duckdb-go/v2"
)

// Config 是连接参数。Endpoint 取 "host:port"，允许带 http:// 前缀（会被剥离，
// Quack over TLS 走服务端配置，本客户端按明文 HTTP 连接，与服务端 disable_ssl=true 对齐）。
type Config struct {
	Endpoint string
	Token    string
}

// Conn 是一条到 quack 服务端的客户端连接。
//
// 内部持有一个 sql.DB（DuckDB 内嵌引擎实例 + 一条 ATTACH）。因 ATTACH 状态与内嵌
// 实例绑定，连接池被限制为 1 —— 并发查询会串行化。监控侧 QPS 低（MCP 查询按需、
// 批量写入已由调用方串行化），该取舍换取「ATTACH 只做一次」与确定的会话边界。
type Conn struct {
	db  *sql.DB
	cfg Config

	mu sync.Mutex // 串行化重连
}

// Dial 建立连接并完成 ATTACH（含 quack 扩展加载，首次约 100–250ms）。
func Dial(ctx context.Context, cfg Config) (*Conn, error) {
	c := &Conn{cfg: cfg}
	if err := c.open(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// open 打开内嵌引擎并对远端做 ATTACH。
func (c *Conn) open(ctx context.Context) error {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return fmt.Errorf("quack open: %w", err)
	}
	// ATTACH 绑定在内嵌实例上；单连接避免多连接各自一份 catalog 状态。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	host := normalizeEndpoint(c.cfg.Endpoint)
	stmt := fmt.Sprintf("ATTACH 'quack:%s' AS r (TYPE quack, TOKEN %s, DISABLE_SSL true)",
		host, sqlLiteral(c.cfg.Token))
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		_ = db.Close()
		return fmt.Errorf("quack attach %s: %w", host, err)
	}
	c.db = db
	return nil
}

// Query 在远端执行一条 SQL 并把结果读成行（列值已转 Go 原生类型）。
// 传输/目录类错误会自动重连并重试一次；SQL 语义错误原样返回（调用方据此保持 pending）。
func (c *Conn) Query(ctx context.Context, sqlText string) ([][]any, error) {
	vals, err := c.query(ctx, sqlText)
	if err == nil || !isTransportErr(err) {
		return vals, err
	}
	if rerr := c.reconnect(ctx); rerr != nil {
		return nil, err // 重连都失败，优先暴露原始错误
	}
	return c.query(ctx, sqlText)
}

// Exec 执行一条不需要结果集的 SQL（建表/批量写入等）。
func (c *Conn) Exec(ctx context.Context, sqlText string) error {
	_, err := c.Query(ctx, sqlText)
	return err
}

// query 是 Query 的实际执行体。
func (c *Conn) query(ctx context.Context, sqlText string) ([][]any, error) {
	rows, err := c.db.QueryContext(ctx, "SELECT * FROM r.query("+sqlLiteral(sqlText)+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := make([][]any, 0, 16)
	for rows.Next() {
		line := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range line {
			ptrs[i] = &line[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		out = append(out, line)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// reconnect 丢弃旧实例并重新 ATTACH（服务端重启、会话被回收等场景）。
func (c *Conn) reconnect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db != nil {
		_ = c.db.Close()
		c.db = nil
	}
	return c.open(ctx)
}

// Close 释放内嵌引擎与连接。
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil {
		return nil
	}
	err := c.db.Close()
	c.db = nil
	return err
}

// sqlLiteral 生成 DuckDB 字符串字面量（单引号翻倍转义）。
// 用于把任意 SQL 文本作为 r.query() 的参数传入。
func sqlLiteral(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

// normalizeEndpoint 接受 "host:port" 或 "http(s)://host:port"，统一成 host:port。
func normalizeEndpoint(ep string) string {
	ep = strings.TrimSpace(ep)
	ep = strings.TrimPrefix(ep, "http://")
	ep = strings.TrimPrefix(ep, "https://")
	ep = strings.TrimSuffix(ep, "/")
	return ep
}

// isTransportErr 判断错误是否属于「连接/目录失效」这一类，可安全重连重试。
// 只匹配传输层与 catalog 失效；SQL 语义错误（Binder/Parser Error 等）不重试。
func isTransportErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, pat := range []string{
		"Could not connect",
		"Failed to send message",
		"HTTP Error",
		"connection reset",
		"connection refused",
		"EOF",
		"no such catalog",
		"Catalog has been invalidated",
		"Unsupported protocol",
	} {
		if strings.Contains(msg, pat) {
			return true
		}
	}
	return false
}

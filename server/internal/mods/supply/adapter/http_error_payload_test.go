package adapter

import "testing"

// 代理协议字符串错误码不能丢失，其他协议的既有错误优先级仍须保持。
func TestParseErrorPayloadProtocolCompatibility(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		code    string
		message string
	}{
		{"代理套餐不存在", `{"code":"plan_not_found","msg":"套餐不存在"}`, "plan_not_found", "套餐不存在"},
		{"代理备用消息字段", `{"code":"invalid_plan","message":"无效套餐"}`, "invalid_plan", "无效套餐"},
		{"标准错误码优先", `{"error_code":"no_stock","error_message":"库存不足","code":"ignored","msg":"其他"}`, "no_stock", "库存不足"},
		{"短信错误码优先", `{"reason":"upstream_busy","message":"繁忙","code":"ignored"}`, "upstream_busy", "繁忙"},
		{"数字错误码兼容", `{"code":400,"msg":"请求失败"}`, "", "请求失败"},
		{"单独消息兼容", `{"msg":"请求失败"}`, "", "请求失败"},
		{"非结构化错误", `bad gateway`, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, message := parseErrorPayload([]byte(tc.body))
			if code != tc.code || message != tc.message {
				t.Fatalf("want (%q,%q), got (%q,%q)", tc.code, tc.message, code, message)
			}
		})
	}
}

package gateway

import "testing"

func TestAPIGEndpoint(t *testing.T) {
	if got := apigEndpoint("cn-hangzhou", ""); got != "apig.cn-hangzhou.aliyuncs.com" {
		t.Fatalf("default endpoint = %q", got)
	}
	if got := apigEndpoint("cn-hangzhou", " apig-vpc.cn-hangzhou.aliyuncs.com "); got != "apig-vpc.cn-hangzhou.aliyuncs.com" {
		t.Fatalf("override endpoint = %q", got)
	}
}

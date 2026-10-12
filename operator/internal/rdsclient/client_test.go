package rdsclient

import (
	"encoding/xml"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSignKnownRequest(t *testing.T) {
	body := []byte("Action=DescribeDBProxies&DBProxyName=example&Version=2014-10-31")
	req, err := http.NewRequest(http.MethodPost, "https://127.0.0.1:6080/aws/rds", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	when := time.Date(2026, 10, 12, 1, 2, 3, 0, time.UTC)
	sign(req, body, "us-east-1", Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "example-signing-key"}, when)
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261012/us-east-1/rds/aws4_request, SignedHeaders=content-type;host;x-amz-date, Signature=9b152851f6c0c4e81019a9d0fdb77ef0d473486db3b8c0ab7c814ffcfd62192c"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
}

func TestDescribeResponseShapes(t *testing.T) {
	proxyXML := `<DescribeDBProxiesResponse xmlns="http://rds.amazonaws.com/doc/2014-10-31/"><DescribeDBProxiesResult><DBProxies><member><DBProxyName>orders</DBProxyName><DBProxyArn>arn:proxy</DBProxyArn><EngineFamily>MYSQL</EngineFamily><Auth><member><AuthScheme>SECRETS</AuthScheme><SecretArn>arn:secret</SecretArn></member></Auth><VpcSecurityGroupIds><member>sg-a</member></VpcSecurityGroupIds></member></DBProxies></DescribeDBProxiesResult></DescribeDBProxiesResponse>`
	var proxies describeProxiesResponse
	if err := xml.Unmarshal([]byte(proxyXML), &proxies); err != nil {
		t.Fatal(err)
	}
	if len(proxies.Items) != 1 || proxies.Items[0].Name != "orders" || len(proxies.Items[0].Auth) != 1 || proxies.Items[0].Auth[0].SecretARN != "arn:secret" {
		t.Fatalf("decoded proxies = %+v", proxies.Items)
	}

	tagXML := `<ListTagsForResourceResponse xmlns="http://rds.amazonaws.com/doc/2014-10-31/"><ListTagsForResourceResult><TagList><Tag><Key>team</Key><Value>payments</Value></Tag></TagList></ListTagsForResourceResult></ListTagsForResourceResponse>`
	var tags listTagsResponse
	if err := xml.Unmarshal([]byte(tagXML), &tags); err != nil {
		t.Fatal(err)
	}
	if len(tags.Items) != 1 || tags.Items[0].Key != "team" {
		t.Fatalf("decoded tags = %+v", tags.Items)
	}
}

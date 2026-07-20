package credprovider

import (
	"context"

	credential "github.com/aliyun/credentials-go/credentials"
)

// NewAliyunCredential adapts a TokenManager into the
// github.com/aliyun/credentials-go Credential interface, which is what
// Alibaba Cloud SDK clients (APIG, OSS, STS, ...) consume.
//
// Each call the SDK makes to GetCredential() triggers TokenManager.Token,
// which transparently refreshes the STS triple when it is about to
// expire. SDK clients therefore see a credential that never goes stale
// so long as the sidecar stays reachable.
//
// Note: the SDK may call the Get* methods off-ctx; we bridge to a
// background context in those cases. The real work happens in
// GetCredential which the SDK calls before signing each request.
func NewAliyunCredential(tm *TokenManager) credential.Credential {
	return &aliyunCredential{tm: tm}
}

type aliyunCredential struct {
	tm *TokenManager
}

// credentialType must match one of the types the Aliyun SDK knows about so
// that SDK-internal code (particularly gateway-pop's credential type
// dispatch) treats the returned security token correctly. "sts" is the
// sentinel used for AK + SK + SecurityToken triples.
const credentialType = "sts"

func (a *aliyunCredential) token() (*IssueResponse, error) {
	return a.tm.Token(context.Background())
}

func (a *aliyunCredential) GetAccessKeyId() (*string, error) {
	t, err := a.token()
	if err != nil {
		return nil, err
	}
	return strPtr(t.AccessKeyID), nil
}

func (a *aliyunCredential) GetAccessKeySecret() (*string, error) {
	t, err := a.token()
	if err != nil {
		return nil, err
	}
	return strPtr(t.AccessKeySecret), nil
}

func (a *aliyunCredential) GetSecurityToken() (*string, error) {
	t, err := a.token()
	if err != nil {
		return nil, err
	}
	return strPtr(t.SecurityToken), nil
}

func (a *aliyunCredential) GetBearerToken() *string {
	empty := ""
	return &empty
}

func (a *aliyunCredential) GetType() *string {
	t := credentialType
	return &t
}

func (a *aliyunCredential) GetCredential() (*credential.CredentialModel, error) {
	t, err := a.token()
	if err != nil {
		return nil, err
	}
	typ := credentialType
	provider := "hiclaw-credential-provider"
	return &credential.CredentialModel{
		AccessKeyId:     strPtr(t.AccessKeyID),
		AccessKeySecret: strPtr(t.AccessKeySecret),
		SecurityToken:   strPtr(t.SecurityToken),
		Type:            &typ,
		ProviderName:    &provider,
	}, nil
}

func strPtr(s string) *string { return &s }

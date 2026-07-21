package credentials

// STSToken holds temporary credentials issued to a worker.
type STSToken struct {
	AccessKeyID     string `json:"access_key_id"`
	AccessKeySecret string `json:"access_key_secret"`
	SecurityToken   string `json:"security_token"`
	Expiration      string `json:"expiration"`
	ExpiresInSec    int    `json:"expires_in_sec"`
	OSSEndpoint     string `json:"oss_endpoint"`
	OSSBucket       string `json:"oss_bucket"`
}

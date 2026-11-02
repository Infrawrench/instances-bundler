package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// The GCP reader needs an OAuth access token: the Cloud Billing Catalog API refuses anonymous callers, and
// listing machine types needs a project. To keep the module free of dependencies, gcpCredentials does the
// two token exchanges it needs itself. In CI, the publish workflow gets a token through workload identity
// federation and passes it as GCP_ACCESS_TOKEN, so no key is stored anywhere (see README.md).

const gcpScopes = "https://www.googleapis.com/auth/cloud-billing.readonly https://www.googleapis.com/auth/compute.readonly"

const gcpTokenURL = "https://oauth2.googleapis.com/token"

// gcpCreds is an access token and the project to list machine types in.
type gcpCreds struct {
	Token   string
	Project string
}

// gcpKeyFile is a service account key or an authorized user file (what `gcloud auth application-default
// login` writes).
type gcpKeyFile struct {
	Type           string `json:"type"`
	ProjectID      string `json:"project_id"`
	PrivateKey     string `json:"private_key"`
	ClientEmail    string `json:"client_email"`
	TokenURI       string `json:"token_uri"`
	ClientID       string `json:"client_id"`
	ClientSecret   string `json:"client_secret"`
	RefreshToken   string `json:"refresh_token"`
	QuotaProjectID string `json:"quota_project_id"`
}

// gcpCredentials finds credentials, in this order:
//
//   - GCP_ACCESS_TOKEN, a token from elsewhere (such as `gcloud auth print-access-token`);
//   - GCP_SERVICE_ACCOUNT_KEY, the contents of a service account key;
//   - GOOGLE_APPLICATION_CREDENTIALS, the path of a service account key or authorized user file;
//   - gcloud's application default credentials file.
//
// The project is GCP_PROJECT if set, otherwise the key's project.
func gcpCredentials(ctx context.Context, client *http.Client) (gcpCreds, error) {
	project := os.Getenv("GCP_PROJECT")
	if tok := os.Getenv("GCP_ACCESS_TOKEN"); tok != "" {
		if project == "" {
			return gcpCreds{}, fmt.Errorf("GCP_ACCESS_TOKEN needs GCP_PROJECT, the project to list machine types in")
		}
		return gcpCreds{Token: tok, Project: project}, nil
	}
	var data []byte
	var from string
	switch {
	case os.Getenv("GCP_SERVICE_ACCOUNT_KEY") != "":
		data, from = []byte(os.Getenv("GCP_SERVICE_ACCOUNT_KEY")), "GCP_SERVICE_ACCOUNT_KEY"
	case os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") != "":
		from = os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	default:
		home, err := os.UserHomeDir()
		if err != nil {
			return gcpCreds{}, err
		}
		from = home + "/.config/gcloud/application_default_credentials.json"
	}
	if data == nil {
		var err error
		if data, err = os.ReadFile(from); err != nil {
			return gcpCreds{}, fmt.Errorf("no GCP credentials: set GCP_SERVICE_ACCOUNT_KEY or GOOGLE_APPLICATION_CREDENTIALS (%w)", err)
		}
	}
	var key gcpKeyFile
	if err := json.Unmarshal(data, &key); err != nil {
		return gcpCreds{}, fmt.Errorf("%s: %w", from, err)
	}
	if project == "" {
		project = cmpOr(key.ProjectID, key.QuotaProjectID)
	}
	if project == "" {
		return gcpCreds{}, fmt.Errorf("%s names no project; set GCP_PROJECT", from)
	}
	var form url.Values
	tokenURL := cmpOr(key.TokenURI, gcpTokenURL)
	switch key.Type {
	case "service_account":
		assertion, err := gcpAssertion(key, tokenURL, time.Now())
		if err != nil {
			return gcpCreds{}, fmt.Errorf("%s: %w", from, err)
		}
		form = url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {assertion}}
	case "authorized_user":
		form = url.Values{"grant_type": {"refresh_token"}, "refresh_token": {key.RefreshToken},
			"client_id": {key.ClientID}, "client_secret": {key.ClientSecret}}
	default:
		return gcpCreds{}, fmt.Errorf("%s: credentials of type %q are not supported", from, key.Type)
	}
	tok, err := gcpExchange(ctx, client, tokenURL, form)
	if err != nil {
		return gcpCreds{}, fmt.Errorf("%s: %w", from, err)
	}
	return gcpCreds{Token: tok, Project: project}, nil
}

// gcpAssertion signs the JWT a service account trades for an access token.
func gcpAssertion(key gcpKeyFile, aud string, now time.Time) (string, error) {
	block, _ := pem.Decode([]byte(key.PrivateKey))
	if block == nil {
		return "", fmt.Errorf("private_key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		if parsed, err = x509.ParsePKCS1PrivateKey(block.Bytes); err != nil {
			return "", fmt.Errorf("private_key: %w", err)
		}
	}
	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return "", fmt.Errorf("private_key is not an RSA key")
	}
	enc := base64.RawURLEncoding
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{"iss": key.ClientEmail, "scope": gcpScopes, "aud": aud,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix()})
	signed := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, rsaKey, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signed + "." + enc.EncodeToString(sig), nil
}

func gcpExchange(ctx context.Context, client *http.Client, tokenURL string, form url.Values) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.Unmarshal(data, &out)
	if resp.StatusCode != http.StatusOK || out.AccessToken == "" {
		return "", fmt.Errorf("getting an access token: %s %s %s", resp.Status, out.Error, out.Description)
	}
	return out.AccessToken, nil
}

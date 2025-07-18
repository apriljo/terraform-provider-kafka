package kafka

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/IBM/sarama"
	"github.com/aws/aws-msk-iam-sasl-signer-go/signer"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/endpointcreds"
	"golang.org/x/net/proxy"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

type Config struct {
	BootstrapServers                       *[]string
	Timeout                                int
	CACert                                 string
	ClientCert                             string
	ClientCertKey                          string
	ClientCertKeyPassphrase                string
	KafkaVersion                           string
	TLSEnabled                             bool
	SkipTLSVerify                          bool
	SASLUsername                           string
	SASLPassword                           string
	SASLMechanism                          string
	SASLAWSContainerAuthorizationTokenFile string
	SASLAWSContainerCredentialsFullUri     string
	SASLAWSRegion                          string
	SASLAWSRoleArn                         string
	SASLAWSExternalId                      string
	SASLAWSProfile                         string
	SASLAWSAccessKey                       string
	SASLAWSSecretKey                       string
	SASLAWSToken                           string
	SASLAWSCredsDebug                      bool
	SASLTokenUrl                           string
	SASLAWSSharedConfigFiles               *[]string
	SASLOAuthScopes                        []string
}

type OAuth2Config interface {
	Token(ctx context.Context) (*oauth2.Token, error)
}

type oauthbearerTokenProvider struct {
	tokenExpiration time.Time
	token           string
	oauth2Config    OAuth2Config
}

func newOauthbearerTokenProvider(oauth2Config OAuth2Config) *oauthbearerTokenProvider {
	return &oauthbearerTokenProvider{
		tokenExpiration: time.Time{},
		token:           "",
		oauth2Config:    oauth2Config,
	}
}

func (o *oauthbearerTokenProvider) Token() (*sarama.AccessToken, error) {
	var accessToken string
	var err error
	currentTime := time.Now()
	ctx := context.Background()

	if o.token != "" && currentTime.Before(o.tokenExpiration.Add(time.Duration(-2)*time.Second)) {
		accessToken = o.token
		err = nil
	} else {
		token, _err := o.oauth2Config.Token(ctx)
		err = _err
		if err == nil {
			accessToken = token.AccessToken
			o.token = token.AccessToken
			o.tokenExpiration = token.Expiry
		}
	}

	return &sarama.AccessToken{Token: accessToken}, err
}

func (c *Config) Token() (*sarama.AccessToken, error) {
	signer.AwsDebugCreds = c.SASLAWSCredsDebug
	var token string
	var err error

	if c.SASLAWSContainerAuthorizationTokenFile != "" && c.SASLAWSContainerCredentialsFullUri != "" {
		log.Printf("[INFO] Generating auth token using container credentials in '%s'", c.SASLAWSRegion)
		var containerAuthorizationToken []byte
		containerAuthorizationToken, err = os.ReadFile(c.SASLAWSContainerAuthorizationTokenFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read authorization token file: %w", err)
		}
		tokenOpt := func(o *endpointcreds.Options) {
			o.AuthorizationToken = string(containerAuthorizationToken)
		}
		credProvider := endpointcreds.New(c.SASLAWSContainerCredentialsFullUri, tokenOpt)
		token, _, err = signer.GenerateAuthTokenFromCredentialsProvider(context.TODO(), c.SASLAWSRegion, credProvider)
	} else if c.SASLAWSRoleArn != "" {
		log.Printf("[INFO] Generating auth token with a role '%s' in '%s'", c.SASLAWSRoleArn, c.SASLAWSRegion)
		token, _, err = signer.GenerateAuthTokenFromRoleWithExternalId(context.TODO(), c.SASLAWSRegion, c.SASLAWSRoleArn, "terraform-kafka-provider", c.SASLAWSExternalId)
	} else if c.SASLAWSProfile != "" {
		if c.SASLAWSSharedConfigFiles != nil && len(*c.SASLAWSSharedConfigFiles) > 0 {
			log.Printf("[INFO] Generating auth token using profile '%s', shared config files '%s' in '%s'", c.SASLAWSProfile, strings.Join(*c.SASLAWSSharedConfigFiles, ","), c.SASLAWSRegion)
			token, _, err = signer.GenerateAuthTokenFromProfileWithSharedConfigFiles(context.TODO(), c.SASLAWSRegion, c.SASLAWSProfile, *c.SASLAWSSharedConfigFiles)
		} else {
			log.Printf("[INFO] Generating auth token using profile '%s' in '%s'", c.SASLAWSProfile, c.SASLAWSRegion)
			token, _, err = signer.GenerateAuthTokenFromProfile(context.TODO(), c.SASLAWSRegion, c.SASLAWSProfile)
		}
	} else if c.SASLAWSAccessKey != "" && c.SASLAWSSecretKey != "" {
		log.Printf("[INFO] Generating auth token using static credentials in '%s'", c.SASLAWSRegion)
		token, _, err = signer.GenerateAuthTokenFromCredentialsProvider(context.TODO(), c.SASLAWSRegion, credentials.NewStaticCredentialsProvider(c.SASLAWSAccessKey, c.SASLAWSSecretKey, c.SASLAWSToken))
	} else {
		log.Printf("[INFO] Generating auth token in '%s'", c.SASLAWSRegion)
		token, _, err = signer.GenerateAuthToken(context.TODO(), c.SASLAWSRegion)
	}
	return &sarama.AccessToken{Token: token}, err
}

func (c *Config) newKafkaConfig() (*sarama.Config, error) {
	kafkaConfig := sarama.NewConfig()

	if c.KafkaVersion != "" {
		version, err := sarama.ParseKafkaVersion(c.KafkaVersion)
		if err != nil {
			return kafkaConfig, fmt.Errorf("error parsing kafka version '%s': %w", c.KafkaVersion, err)
		}
		kafkaConfig.Version = version
	} else {
		kafkaConfig.Version = sarama.V2_7_0_0
	}

	kafkaConfig.ClientID = "terraform-provider-kafka"
	kafkaConfig.Admin.Timeout = time.Duration(c.Timeout) * time.Second
	kafkaConfig.Metadata.Full = true // the default, but just being clear
	kafkaConfig.Metadata.AllowAutoTopicCreation = false

	kafkaConfig.Net.Proxy.Enable = true
	kafkaConfig.Net.Proxy.Dialer = proxy.FromEnvironment()

	kafkaConfig.Net.ReadTimeout = time.Duration(c.Timeout) * time.Second
	kafkaConfig.Net.WriteTimeout = time.Duration(c.Timeout) * time.Second
	kafkaConfig.Metadata.Timeout = time.Duration(c.Timeout) * time.Second

	if c.saslEnabled() {
		switch c.SASLMechanism {
		case "scram-sha512":
			kafkaConfig.Net.SASL.SCRAMClientGeneratorFunc = func() sarama.SCRAMClient { return &XDGSCRAMClient{HashGeneratorFcn: SHA512} }
			kafkaConfig.Net.SASL.Mechanism = sarama.SASLMechanism(sarama.SASLTypeSCRAMSHA512)
		case "scram-sha256":
			kafkaConfig.Net.SASL.SCRAMClientGeneratorFunc = func() sarama.SCRAMClient { return &XDGSCRAMClient{HashGeneratorFcn: SHA256} }
			kafkaConfig.Net.SASL.Mechanism = sarama.SASLMechanism(sarama.SASLTypeSCRAMSHA256)
		case "aws-iam":
			kafkaConfig.Net.SASL.Mechanism = sarama.SASLMechanism(sarama.SASLTypeOAuth)
			region := c.SASLAWSRegion
			if region == "" {
				region = os.Getenv("AWS_REGION")
			}
			if region == "" {
				log.Fatalf("[ERROR] aws region must be configured or AWS_REGION environment variable must be set to use aws-iam sasl mechanism")
			}
			kafkaConfig.Net.SASL.TokenProvider = c
		case "oauthbearer":
			kafkaConfig.Net.SASL.Mechanism = sarama.SASLMechanism(sarama.SASLTypeOAuth)
			tokenUrl := c.SASLTokenUrl
			if tokenUrl == "" {
				tokenUrl = os.Getenv("TOKEN_URL")
			}
			if tokenUrl == "" {
				log.Fatalf("[ERROR] token url must be configured or TOKEN_URL environment variable must be set to use oauthbearer sasl mechanism")
			}
			oauth2Config := clientcredentials.Config{
				TokenURL:     tokenUrl,
				ClientID:     c.SASLUsername,
				ClientSecret: c.SASLPassword,
				Scopes:       c.SASLOAuthScopes,
			}
			kafkaConfig.Net.SASL.TokenProvider = newOauthbearerTokenProvider(&oauth2Config)
		case "plain":
		default:
			log.Fatalf("[ERROR] Invalid sasl mechanism \"%s\": can only be \"scram-sha256\", \"scram-sha512\", \"aws-iam\" or \"plain\"", c.SASLMechanism)
		}

		kafkaConfig.Net.SASL.Enable = true
		kafkaConfig.Net.SASL.Handshake = true

		if c.SASLUsername != "" {
			kafkaConfig.Net.SASL.User = c.SASLUsername
		}
		if c.SASLPassword != "" {
			kafkaConfig.Net.SASL.Password = c.SASLPassword
		}
	} else {
		log.Printf("[WARN] SASL disabled username: '%s', password '%s'", c.SASLUsername, "****")
	}

	if c.TLSEnabled {
		tlsConfig, err := newTLSConfig(
			c.ClientCert,
			c.ClientCertKey,
			c.CACert,
			c.ClientCertKeyPassphrase,
		)
		if err != nil {
			return kafkaConfig, err
		}

		kafkaConfig.Net.TLS.Enable = true
		kafkaConfig.Net.TLS.Config = tlsConfig
		kafkaConfig.Net.TLS.Config.InsecureSkipVerify = c.SkipTLSVerify
	}

	return kafkaConfig, nil
}

func (c *Config) saslEnabled() bool {
	return c.SASLUsername != "" || c.SASLPassword != "" || c.SASLMechanism == "aws-iam"
}

func NewTLSConfig(clientCert, clientKey, caCert, clientKeyPassphrase string) (*tls.Config, error) {
	return newTLSConfig(clientCert, clientKey, caCert, clientKeyPassphrase)
}

func parsePemOrLoadFromFile(input string) (*pem.Block, []byte, error) {
	// attempt to parse
	inputBytes := []byte(input)
	inputBlock, _ := pem.Decode(inputBytes)

	if inputBlock == nil {
		// attempt to load from file
		log.Printf("[INFO] Attempting to load from file '%s'", input)
		var err error
		inputBytes, err = os.ReadFile(input)
		if err != nil {
			return nil, nil, err
		}
		inputBlock, _ = pem.Decode(inputBytes)
		if inputBlock == nil {
			return nil, nil, fmt.Errorf("[ERROR] Error unable to decode pem")
		}
	}
	return inputBlock, inputBytes, nil
}

func newTLSConfig(clientCert, clientKey, caCert, clientKeyPassphrase string) (*tls.Config, error) {
	tlsConfig := tls.Config{}

	if clientCert != "" && clientKey != "" {
		_, certBytes, err := parsePemOrLoadFromFile(clientCert)
		if err != nil {
			log.Printf("[ERROR] Unable to read certificate %s", err)
			return &tlsConfig, err
		}

		keyBlock, keyBytes, err := parsePemOrLoadFromFile(clientKey)
		if err != nil {
			log.Printf("[ERROR] Unable to read private key %s", err)
			return &tlsConfig, err
		}

		if x509.IsEncryptedPEMBlock(keyBlock) { //nolint:staticcheck
			log.Printf("[INFO] Using encrypted private key")
			var err error

			keyBytes, err = x509.DecryptPEMBlock(keyBlock, []byte(clientKeyPassphrase)) //nolint:staticcheck
			if err != nil {
				log.Printf("[ERROR] Error decrypting private key with passphrase %s", err)
				return &tlsConfig, err
			}
			keyBytes = pem.EncodeToMemory(&pem.Block{
				Type:  keyBlock.Type,
				Bytes: keyBytes,
			})
		}

		cert, err := tls.X509KeyPair(certBytes, keyBytes)
		if err != nil {
			log.Printf("[ERROR] Error creating X509KeyPair %s", err)
			return &tlsConfig, err
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	if caCert == "" {
		log.Println("[WARN] no CA file set skipping")
		return &tlsConfig, nil
	}

	caCertPool, _ := x509.SystemCertPool()
	if caCertPool == nil {
		caCertPool = x509.NewCertPool()
	}

	_, caBytes, err := parsePemOrLoadFromFile(caCert)
	if err != nil {
		log.Printf("[ERROR] Unable to read CA %s", err)
		return &tlsConfig, err
	}
	ok := caCertPool.AppendCertsFromPEM(caBytes)
	log.Printf("[TRACE] set cert pool %v", ok)
	if !ok {
		return &tlsConfig, fmt.Errorf("could not add the caPem")
	}

	tlsConfig.RootCAs = caCertPool
	return &tlsConfig, nil
}

func (config *Config) copyWithMaskedSensitiveValues() Config {
	copy := Config{
		config.BootstrapServers,
		config.Timeout,
		config.CACert,
		config.ClientCert,
		"*****",
		"*****",
		config.KafkaVersion,
		config.TLSEnabled,
		config.SkipTLSVerify,
		config.SASLUsername,
		"*****",
		config.SASLMechanism,
		config.SASLAWSContainerAuthorizationTokenFile,
		config.SASLAWSContainerCredentialsFullUri,
		config.SASLAWSRegion,
		config.SASLAWSRoleArn,
		"*****",
		config.SASLAWSProfile,
		config.SASLAWSAccessKey,
		"*****",
		config.SASLAWSToken,
		config.SASLAWSCredsDebug,
		config.SASLTokenUrl,
		config.SASLAWSSharedConfigFiles,
		config.SASLOAuthScopes,
	}
	return copy
}

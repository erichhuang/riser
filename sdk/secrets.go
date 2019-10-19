package sdk

import (
	"fmt"
	"net/http"

	"github.com/tshak/riser-server/api/v1/model"
)

type SecretsClient interface {
	List(appName, stageName string) ([]model.SecretMetaStatus, error)
	Save(appName, stageName, secretName, plainTextSecret string) error
}

type secretsClient struct {
	client *Client
}

func (c *secretsClient) List(appName, stageName string) ([]model.SecretMetaStatus, error) {
	request, err := c.client.NewGetRequest(fmt.Sprintf("/api/v1/secrets/%s/%s", appName, stageName))
	if err != nil {
		return nil, err
	}

	secretMetas := []model.SecretMetaStatus{}
	_, err = c.client.Do(request, &secretMetas)
	if err != nil {
		return nil, err
	}
	return secretMetas, nil
}

func (c *secretsClient) Save(appName, stageName, secretName, plainTextSecret string) error {
	unsealed := model.UnsealedSecret{
		SecretMeta: model.SecretMeta{
			App:   appName,
			Stage: stageName,
			Name:  secretName,
		},
		PlainText: plainTextSecret,
	}
	request, err := c.client.NewRequest(http.MethodPut, "/api/v1/secrets", unsealed)
	if err != nil {
		return err
	}

	_, err = c.client.Do(request, nil)
	return err
}
/*

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package utils

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type (
	// nimCatalogQuery is used for constructing a query for NIM catalog fetch
	nimCatalogQuery struct {
		Query string `json:"query"`
		Page  int    `json:"page"`
	}

	// nimCatalogResponse represents the NIM catalog response
	nimCatalogResponse struct {
		Results []struct {
			GroupValue string `json:"groupValue"`
			Resources  []struct {
				ResourceId string `json:"resourceId"`
				Attributes []struct {
					Key   string `json:"key"`
					Value string `json:"value"`
				} `json:"attributes"`
			} `json:"resources"`
		} `json:"results"`
	}

	nimTokenResponse struct {
		Token     string `json:"token"`
		ExpiresIn int    `json:"expires_in"`
	}

	// NimImage is a representation of a NIM custom runtime image
	NimImage struct {
		Name    string
		Version string
	}
)

const (
	nimGetCatalog     = "https://api.ngc.nvidia.com/v2/search/catalog/resources/CONTAINER"
	nimGetTokenFmt    = "https://nvcr.io/proxy_auth?account=$oauthtoken&offline_token=true&scope=repository:%s:pull"
	nimGetManifestFmt = "https://nvcr.io/v2/%s/manifests/%s"
)

var httpClient http.Client

func init() {
	httpClient = http.Client{}
}

// GetAvailableNimImageList is used to fetch a list of available NIM custom runtime images
func GetAvailableNimImageList() ([]NimImage, error) {
	req, reqErr := http.NewRequest("GET", nimGetCatalog, nil)
	if reqErr != nil {
		return nil, reqErr
	}

	params, _ := json.Marshal(nimCatalogQuery{Query: "orgName:nim", Page: 0})
	query := req.URL.Query()
	query.Add("q", string(params))

	req.URL.RawQuery = query.Encode()

	resp, respErr := httpClient.Do(req)
	if respErr != nil {
		return nil, respErr
	}

	body, bodyErr := io.ReadAll(resp.Body)
	if bodyErr != nil {
		return nil, bodyErr
	}

	catRes := &nimCatalogResponse{}
	if err := json.Unmarshal(body, catRes); err != nil {
		return nil, err
	}

	return mapNimCatalogResponseToImageList(catRes), nil
}

// ValidateApiKey will verify the given API key can pull the given custom runtime image
func ValidateApiKey(apiKey string, image NimImage) error {
	tokenResp, tokenErr := getToken(apiKey, image.Name)
	if tokenErr != nil {
		return tokenErr
	}

	manifestErr := attemptToPullManifest(image, tokenResp)
	if manifestErr != nil {
		return manifestErr
	}

	return nil
}

func mapNimCatalogResponseToImageList(resp *nimCatalogResponse) []NimImage {
	var images []NimImage
	for _, result := range resp.Results {
		if result.GroupValue == "CONTAINER" {
			for _, resource := range result.Resources {
				for _, attribute := range resource.Attributes {
					if attribute.Key == "latestTag" {
						images = append(images, NimImage{
							Name:    resource.ResourceId,
							Version: attribute.Value,
						})
						break
					}
				}
			}
		}
	}
	return images
}

func getToken(apiKey, repo string) (*nimTokenResponse, error) {
	req, reqErr := http.NewRequest("GET", fmt.Sprintf(nimGetTokenFmt, repo), nil)
	if reqErr != nil {
		return nil, reqErr
	}

	encoded := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("$oauthtoken:%s", apiKey)))
	req.Header.Add("Authorization", fmt.Sprintf("Basic %s", encoded))

	resp, respErr := httpClient.Do(req)
	if respErr != nil {
		return nil, respErr
	}

	body, bodyErr := io.ReadAll(resp.Body)
	if bodyErr != nil {
		return nil, bodyErr
	}

	tokenResponse := &nimTokenResponse{}
	if err := json.Unmarshal(body, tokenResponse); err != nil {
		return nil, err
	}

	return tokenResponse, nil
}

func attemptToPullManifest(image NimImage, tokenResp *nimTokenResponse) error {
	req, reqErr := http.NewRequest("GET", fmt.Sprintf(nimGetManifestFmt, image.Name, image.Version), nil)
	if reqErr != nil {
		return reqErr
	}

	req.Header.Add("Authorization", tokenResp.Token)

	resp, respErr := httpClient.Do(req)
	if respErr != nil {
		return respErr
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to pul manifest")
	}

	return nil
}

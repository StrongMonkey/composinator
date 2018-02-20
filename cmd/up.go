package cmd

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/rancher/norman/clientbase"
	"github.com/rancher/norman/types"
	clusterClient "github.com/rancher/types/client/cluster/v3"
	managementClient "github.com/rancher/types/client/management/v3"
	projectClient "github.com/rancher/types/client/project/v3"
	"github.com/rancher/types/compose"
	"github.com/urfave/cli"
	"gopkg.in/yaml.v2"
)

func UpCommand() cli.Command {
	return cli.Command{
		Name:   "up",
		Usage:  "Create Rancher Resources based on yaml files",
		Action: rancherUp,
		Flags: []cli.Flag{
			cli.StringFlag{
				Name:  "file,f",
				Usage: "Specify the yaml file for rancher",
				Value: "./example/example.yml",
			},
			cli.StringFlag{
				Name:  "token",
				Usage: "Token string to construct rancher client",
				Value: "token-2z5sz:5vqmsdgp8wpcq4l5jxmk4tmf7q4wl57vhfkzp9fk2dcpdkxctqmrxt",
			},
			cli.StringFlag{
				Name:  "cacert-file",
				Usage: "cacert cert file path",
			},
			cli.BoolFlag{
				Name: "insecure-skip-tls",
				Usage: "Insecure flag to connect rancher server",
			},
			cli.StringFlag{
				Name:  "url",
				Usage: "rancher server url",
				Value: "https://localhost:8443/v3",
			},
		},
	}
}

func rancherUp(ctx *cli.Context) error {
	fp := ctx.String("file")
	file, err := os.Open(fp)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := ioutil.ReadAll(file)
	if err != nil {
		return err
	}
	foo := &compose.Config{}
	if err := yaml.Unmarshal(data, foo); err != nil {
		return err
	}

	cacert := ""
	if ctx.String("cacert-file") != "" {
		certFilePath := ctx.String("cacert-file")
		cfile, err := os.Open(certFilePath)
		if err != nil {
			return err
		}
		defer cfile.Close()
		data, err = ioutil.ReadAll(cfile)
		if err != nil {
			return err
		}
		cacert = string(data)
	}
	clientset, err := constructClient(ctx.String("token"), cacert, ctx.String("url"), ctx.Bool("insecure-skip-tls"))
	if err != nil {
		return err
	}

	return up(clientset, foo)
}

type ClientSet struct {
	mClient *managementClient.Client
	cClient *clusterClient.Client
	pClient *projectClient.Client
}

var (
	WaitCondition = map[string]func(baseClient *clientbase.APIBaseClient, id, schemaType string) error{
		"cluster": WaitCluster,
	}
)

func WaitCluster(baseClient *clientbase.APIBaseClient, id, schemaType string) error {
	start := time.Now()
	for {
		respObj := managementClient.Cluster{}
		if err := baseClient.ByID(schemaType, id, &respObj); err != nil {
			return err
		}
		for _, cond := range respObj.Conditions {
			if cond.Type == "Ready" && cond.Status == "True" {
				return nil
			}
		}
		time.Sleep(time.Second * 10)
		if time.Now().After(start.Add(time.Minute * 30)) {
			return errors.Errorf("Timeout wait for cluster %s to be ready", id)
		}
	}
}

func up(clientset ClientSet, config *compose.Config) error {
	return CreateResourcesWithReferences(clientset, config)
}

func CreateResourcesWithReferences(clientset ClientSet, config *compose.Config) error {
	// schema map contains all the schemas
	schemas := GetSchemaMap(clientset)

	// referenceMap is a map of schemaType with name -> id value
	referenceMap := map[string]map[string]string{}

	rawData, err := json.Marshal(config)
	if err != nil {
		return err
	}
	rawMap := map[string]interface{}{}
	if err := json.Unmarshal(rawData, &rawMap); err != nil {
		return err
	}
	delete(rawMap, "version")
	// find all resources that has no references
	sortedSchemas := SortSchema(schemas)
	for _, schemaKey := range sortedSchemas {
		key := schemaKey + "s"
		if v, ok := rawMap[key]; ok {
			value, ok := v.(map[string]interface{})
			if !ok {
				continue
			}
			var bClient *clientbase.APIBaseClient
			for name, data := range value {
				dataMap, ok := data.(map[string]interface{})
				if !ok {
					break
				}
				if err := replaceReference(schemaKey, schemas[schemaKey], dataMap, referenceMap, clientset); err != nil {
					return err
				}
				dataMap["name"] = name
				// for cluster-scoped resources, we need a clusterId
				clusterId := ""
				if s, ok := dataMap["clusterId"]; ok {
					clusterId = s.(string)
				}
				// for cluster-scoped resources, we need a projectId
				projectId := ""
				if s, ok := dataMap["projectId"]; ok {
					projectId = s.(string)
				}
				baseClient, err := GetClient(schemaKey, clientset, clusterId, projectId)
				if err != nil {
					return err
				}
				bClient = baseClient
				respObj := map[string]interface{}{}
				created := map[string]string{}
				// if the resource belongs to managemnent, we have to make the name is unique
				if _, ok := clientset.mClient.Types[schemaKey]; ok {
					respObj := map[string]interface{}{}
					if err := baseClient.List(schemaKey, &types.ListOpts{}, &respObj); err != nil {
						return err
					}
					if data, ok := respObj["data"]; ok {
						if collections, ok := data.([]interface{}); ok {
							for _, obj := range collections {
								if objMap, ok := obj.(map[string]interface{}); ok {
									createdName := getValue(objMap, "name")
									if createdName != "" {
										created[createdName] = getValue(objMap, "id")
									}
								}
							}
						}
					}
				}

				id := ""
				if v, ok := created[name]; ok {
					id = v
				} else {
					if err := baseClient.Create(schemaKey, dataMap, &respObj); err != nil && !strings.Contains(err.Error(), "already exist") {
						return err
					} else if err != nil && strings.Contains(err.Error(), "already exist") {
						break
					}
					v, ok := respObj["id"]
					if !ok {
						return errors.Errorf("id is missing after creating %s obj", schemaKey)
					}
					id = v.(string)
				}
				if f, ok := WaitCondition[schemaKey]; ok {
					if err := f(baseClient, id, schemaKey); err != nil {
						return err
					}
				}
			}
			// fill in reference map name -> id
			if bClient == nil {
				continue
			}
			if err := fillInReferenceMap(bClient, schemaKey, referenceMap); err != nil {
				return err
			}
		}
	}
	return nil
}

func fillInReferenceMap(client *clientbase.APIBaseClient, schemaKey string, referenceMap map[string]map[string]string) error {
	if schemaKey == "namespace" {
		return nil
	}
	referenceMap[schemaKey] = map[string]string{}
	respObj := map[string]interface{}{}
	if err := client.List(schemaKey, &types.ListOpts{}, &respObj); err != nil {
		return err
	}
	if data, ok := respObj["data"]; ok {
		if collections, ok := data.([]interface{}); ok {
			for _, obj := range collections {
				if objMap, ok := obj.(map[string]interface{}); ok {
					projectId := getValue(objMap, "projectId")
					clusterNamePrefix := ""
					// if schemaKey is project we prefix cluster name to it
					if schemaKey == "project" {
						parts := strings.Split(getValue(objMap, "id"), ":")
						if len(parts) != 2 {
							return errors.New("Invalid format on projectId")
						}
						cId := parts[0]
						var cluster managementClient.Cluster
						if err := client.ByID("cluster", cId, &cluster); err != nil {
							return err
						}
						clusterNamePrefix = cluster.Name + ":"
					}
					name := clusterNamePrefix + getValue(objMap, "name")
					id := getValue(objMap, "id")
					if name != "" && id != "" {
						if projectId != "" {
							referenceMap[schemaKey][fmt.Sprintf("%s:%s", projectId, name)] = id
						} else {
							referenceMap[schemaKey][name] = id
						}
					}
				}
			}
		}
	}
	return nil
}

func parseClusterAndProjectId(data map[string]interface{}, referenceMap map[string]map[string]string) (string, string, error) {
	projectName := getValue(data, "projectId")
	clusterId, projectId := "", ""
	if projectName != "" {
		parts := strings.Split(projectName, ":")
		if len(parts) == 2 {
			clusterName := parts[0]
			cId, ok := referenceMap["cluster"][clusterName]
			if !ok {
				return "", "", errors.New("failed to find clusterId reference")
			}
			pId, ok := referenceMap["project"][projectName]
			if !ok {
				return "", "", errors.New("failed to find projectId reference")
			}
			clusterId = cId
			projectId = pId
		}
	}
	return clusterId, projectId, nil
}

// replaceReference replace name to id
func replaceReference(schemaType string, schema types.Schema, data map[string]interface{}, referenceMap map[string]map[string]string, clientset ClientSet) error {
	for key, field := range schema.ResourceFields {
		if strings.Contains(field.Type, "reference") {
			reference := getReference(field.Type)
			// for namespace and persistVolume, the name is the id so no need to replace reference
			if reference == "namespace" || reference == "persistVolume" {
				continue
			}
			if _, ok := data[key]; !ok {
				continue
			}
			// if the reference doesn't exist in the reference map, we construct them
			if _, ok := referenceMap[reference]; !ok {
				if _, ok := referenceMap["project"]; !ok {
					if err := fillInReferenceMap(&clientset.mClient.APIBaseClient, "project", referenceMap); err != nil {
						return err
					}
				}
				if _, ok := referenceMap["cluster"]; !ok {
					if err := fillInReferenceMap(&clientset.mClient.APIBaseClient, "cluster", referenceMap); err != nil {
						return err
					}
				}
				clusterId, projectId := "", ""
				// if reference belongs to a project, we need to parse the right clusterId and projectId and construct the client to list resources on inside that project
				if _, ok := clientset.pClient.Types[reference]; ok {
					cId, pId, err := parseClusterAndProjectId(data, referenceMap)
					if err != nil {
						return err
					}
					clusterId = cId
					projectId = pId
				}
				client, err := GetClient(reference, clientset, clusterId, projectId)
				if err != nil {
					return err
				}
				if err := fillInReferenceMap(client, reference, referenceMap); err != nil {
					return err
				}
			}
			// replace clusterName:projectName -> clusterId:ProjectId
			if key == "projectId" {
				cId, pId, err := parseClusterAndProjectId(data, referenceMap)
				if err != nil {
					return err
				}
				data["projectId"] = pId
				data["clusterId"] = cId
				continue
			}

			// if it reference a name inside a project
			if _, ok := clientset.pClient.Types[schemaType]; ok {
				cId, pId, err := parseClusterAndProjectId(data, referenceMap)
				if err != nil {
					return err
				}
				data[key] = referenceMap[fmt.Sprintf("%s:%s:%v", cId, pId, data[key])]
			} else {
				data[key] = referenceMap[reference][data[key].(string)]
			}
		}
	}
	return nil
}

func getValue(data map[string]interface{}, key string) string {
	if v, ok := data[key]; ok {
		if _, ok := v.(string); ok {
			return v.(string)
		}
	}
	return ""
}

func GetClient(schemaType string, clientset ClientSet, clusterId, projectId string) (*clientbase.APIBaseClient, error) {
	var baseClient *clientbase.APIBaseClient
	if _, ok := clientset.mClient.APIBaseClient.Types[schemaType]; ok {
		baseClient = &clientset.mClient.APIBaseClient
	}
	if _, ok := clientset.pClient.APIBaseClient.Types[schemaType]; ok {
		baseClient = &clientset.pClient.APIBaseClient
		if projectId == "" {
			return nil, errors.Errorf("projectId is required for %s", schemaType)
		}
		baseClient.Opts.URL = baseClient.Opts.URL + "/" + projectId
	}
	if _, ok := clientset.cClient.APIBaseClient.Types[schemaType]; ok {
		baseClient = &clientset.cClient.APIBaseClient
		if clusterId == "" {
			return nil, errors.Errorf("clusterId is required for %s", schemaType)
		}
		baseClient.Opts.URL = baseClient.Opts.URL + "/" + clusterId
	}
	return baseClient, nil
}

func GetSchemaMap(clientset ClientSet) map[string]types.Schema {
	schemas := map[string]types.Schema{}
	for k, s := range clientset.mClient.Types {
		if _, ok := s.ResourceFields["creatorId"]; !ok {
			continue
		}
		schemas[k] = s
	}
	for k, s := range clientset.pClient.Types {
		if _, ok := s.ResourceFields["creatorId"]; !ok {
			continue
		}
		schemas[k] = s
	}
	for k, s := range clientset.cClient.Types {
		if _, ok := s.ResourceFields["creatorId"]; !ok {
			continue
		}
		schemas[k] = s
	}
	return schemas
}

func constructClient(token, cacert, url string, insecure bool) (ClientSet, error) {
	pClient, err := projectClient.NewClient(&clientbase.ClientOpts{
		CACerts:  cacert,
		URL:      url + "/projects",
		TokenKey: token,
		Insecure: insecure,
	})
	if err != nil {
		return ClientSet{}, err
	}
	mClient, err := managementClient.NewClient(&clientbase.ClientOpts{
		CACerts:  cacert,
		URL:      url,
		TokenKey: token,
		Insecure: insecure,
	})
	if err != nil {
		return ClientSet{}, err
	}
	cClient, err := clusterClient.NewClient(&clientbase.ClientOpts{
		CACerts:  cacert,
		URL:      url + "/clusters",
		TokenKey: token,
		Insecure: insecure,
	})
	if err != nil {
		return ClientSet{}, err
	}
	return ClientSet{
		pClient: pClient,
		mClient: mClient,
		cClient: cClient,
	}, nil
}

func SortSchema(schemas map[string]types.Schema) []string {
	inserted := map[string]bool{}
	result := []string{}
	for i := 0; i < 100; i++ {
		for k, schema := range schemas {
			if inserted[k] {
				continue
			}
			ready := true
			for fieldName, field := range schema.ResourceFields {
				if strings.Contains(field.Type, "reference") {
					reference := getReference(field.Type)
					if isNamespaceIDRef(reference, k) {
						continue
					}
					if !inserted[reference] && fieldName != "creatorId" && k != reference {
						ready = false
					}
				}
			}
			if ready {
				inserted[k] = true
				result = append(result, k)
			}
		}
	}
	return result
}

var (
	namespacedSchema = map[string]bool{
		"project": true,
	}
)

func isNamespaceIDRef(ref, schemaType string) bool {
	if ref == "namespace" && namespacedSchema[schemaType] {
		return true
	}
	return false
}

func getReference(name string) string {
	name = strings.TrimSuffix(strings.TrimPrefix(name, "array["), "]")
	r := strings.TrimSuffix(strings.TrimPrefix(name, "reference["), "]")
	return strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(r, "/v3/schemas/"), "/v3/clusters/schemas/"), "/v3/projects/schemas/")
}

// Copyright © 2017 The Kubicorn Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package resources

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/digitalocean/godo"
	"github.com/kubicorn/kubicorn/apis/cluster"
	"github.com/kubicorn/kubicorn/cloud"
	"github.com/kubicorn/kubicorn/pkg/compare"
	"github.com/kubicorn/kubicorn/pkg/logger"
	"github.com/kubicorn/kubicorn/pkg/scp"
	"github.com/kubicorn/kubicorn/pkg/script"
	"github.com/kubicorn/kubicorn/pkg/ssh"
)

var _ cloud.Resource = &Droplet{}

type Droplet struct {
	Shared
	Region           string
	Size             string
	Image            string
	Count            int
	SSHFingerprint   string
	BootstrapScripts []string
	ServerPool       *cluster.ServerPool
}

const (
	MasterIPAttempts               = 100
	MasterIPSleepSecondsPerAttempt = 5
	DeleteAttempts                 = 25
	DeleteSleepSecondsPerAttempt   = 3
)

func (r *Droplet) Actual(immutable *cluster.Cluster) (*cluster.Cluster, cloud.Resource, error) {
	logger.Debug("droplet.Actual")
	newResource := &Droplet{
		Shared: Shared{
			Name:    r.Name,
			CloudID: r.ServerPool.Identifier,
		},
	}

	droplets, _, err := Sdk.Client.Droplets.ListByTag(context.TODO(), r.Name, &godo.ListOptions{})
	if err != nil {
		return nil, nil, err
	}
	ld := len(droplets)
	if ld > 0 {
		newResource.Count = len(droplets)

		// Todo (@kris-nova) once we start to test these implementations we really need to work on the droplet logic. Right now we just pick the first one..
		droplet := droplets[0]
		id := strconv.Itoa(droplet.ID)
		newResource.Name = droplet.Name
		newResource.CloudID = id
		newResource.Size = droplet.Size.Slug
		newResource.Image = droplet.Image.Slug
		newResource.Region = droplet.Region.Slug
	}

	newResource.BootstrapScripts = r.ServerPool.BootstrapScripts
	newResource.SSHFingerprint = immutable.ProviderConfig().SSH.PublicKeyFingerprint
	newResource.Name = r.ServerPool.Name
	newResource.Count = r.ServerPool.MaxCount
	newResource.Region = immutable.ProviderConfig().Location

	newCluster := r.immutableRender(newResource, immutable)
	return newCluster, newResource, nil
}

func (r *Droplet) Expected(immutable *cluster.Cluster) (*cluster.Cluster, cloud.Resource, error) {
	logger.Debug("droplet.Expected")
	newResource := &Droplet{
		Shared: Shared{
			Name:    r.Name,
			CloudID: r.ServerPool.Identifier,
		},
		Size:             r.ServerPool.Size,
		Region:           immutable.ProviderConfig().Location,
		Image:            r.ServerPool.Image,
		Count:            r.ServerPool.MaxCount,
		SSHFingerprint:   immutable.ProviderConfig().SSH.PublicKeyFingerprint,
		BootstrapScripts: r.ServerPool.BootstrapScripts,
	}

	newCluster := r.immutableRender(newResource, immutable)
	return newCluster, newResource, nil
}

func (r *Droplet) Apply(actual, expected cloud.Resource, immutable *cluster.Cluster) (*cluster.Cluster, cloud.Resource, error) {
	logger.Debug("droplet.Apply")
	applyResource := expected.(*Droplet)
	isEqual, err := compare.IsEqual(actual.(*Droplet), expected.(*Droplet))
	if err != nil {
		return nil, nil, err
	}
	if isEqual {
		return immutable, applyResource, nil
	}

	//agent := agent.NewAgent()

	masterIpPrivate := ""
	masterIPPublic := ""
	providerConfig := immutable.ProviderConfig()
	if r.ServerPool.Type == cluster.ServerPoolTypeNode {
		found := false
		for i := 0; i < MasterIPAttempts; i++ {
			masterTag := ""
			machineConfigs := immutable.MachineProviderConfigs()
			for _, machineConfig := range machineConfigs {
				serverPool := machineConfig.ServerPool
				if serverPool.Type == cluster.ServerPoolTypeMaster {
					masterTag = serverPool.Name
				}
			}
			if masterTag == "" {
				return nil, nil, fmt.Errorf("Unable to find master tag for master IP")
			}
			droplets, _, err := Sdk.Client.Droplets.ListByTag(context.TODO(), masterTag, &godo.ListOptions{})
			if err != nil {
				logger.Debug("Hanging for master IP.. (%v)", err)
				time.Sleep(time.Duration(MasterIPSleepSecondsPerAttempt) * time.Second)
				continue
			}
			ld := len(droplets)
			if ld == 0 {
				logger.Debug("Hanging for master IP..")
				time.Sleep(time.Duration(MasterIPSleepSecondsPerAttempt) * time.Second)
				continue
			}
			if ld > 1 {
				return nil, nil, fmt.Errorf("Found [%d] droplets for tag [%s]", ld, masterTag)
			}
			droplet := droplets[0]

			masterIPPublic, err = droplet.PublicIPv4()
			if err != nil || masterIPPublic == "" {
				logger.Debug("Hanging for master IP..")
				time.Sleep(time.Duration(MasterIPSleepSecondsPerAttempt) * time.Second)
				continue
			}

			if !immutable.ProviderConfig().Components.ComponentVPN {
				logger.Info("Waiting for Private IP address...")
				masterIpPrivate, err = droplet.PrivateIPv4()
				if err != nil || masterIpPrivate == "" {
					return nil, nil, fmt.Errorf("Unable to detect private IP: %v", err)
				}
				found = true
			} else {
				logger.Info("Setting up VPN on Droplets... this could take a little bit longer...")
				//pubPath := local.Expand(immutable.ProviderConfig().SSH.PublicKeyPath)
				//privPath := strings.Replace(pubPath, ".pub", "", 1)

				client := ssh.NewSSHClient(masterIPPublic, providerConfig.SSH.Port,
											providerConfig.SSH.User, providerConfig.SSH.PublicKeyPath)
				err = client.Connect()
				if err != nil {
					return nil, nil, fmt.Errorf("Unable to connect to SSH: %v", err)
				}

				masterVpnIP, err := scp.ReadBytes(client, "/tmp/.ip")
				if err != nil {
					logger.Debug("Hanging for VPN IP.. /tmp/.ip (%v)", err)
					time.Sleep(time.Duration(MasterIPSleepSecondsPerAttempt) * time.Second)
					continue
				}
				masterIpPrivate = strings.Replace(string(masterVpnIP), "\n", "", -1)
				openvpnConfig, err := scp.ReadBytes(client, "/tmp/clients.conf")
				if err != nil {
					logger.Debug("Hanging for VPN config.. /tmp/clients.ovpn (%v)", err)
					time.Sleep(time.Duration(MasterIPSleepSecondsPerAttempt) * time.Second)
					continue
				}

				openvpnConfigEscaped := strings.Replace(string(openvpnConfig), "\n", "\\n", -1)
				providerConfig.Values.ItemMap["INJECTEDCONF"] = openvpnConfigEscaped
				found = true

				err = client.Close()
				if err != nil {
					return nil, nil, fmt.Errorf("Error closing SSH connection: %v", err)
				}
			}

			providerConfig.Values.ItemMap["INJECTEDMASTER"] = fmt.Sprintf("%s:%s", masterIpPrivate, immutable.ProviderConfig().KubernetesAPI.Port)
			break
		}
		if !found {
			return nil, nil, fmt.Errorf("Unable to find Master IP after defined wait")
		}
	}

	providerConfig.Values.ItemMap["INJECTEDPORT"] = immutable.ProviderConfig().KubernetesAPI.Port
	immutable.SetProviderConfig(providerConfig)

	userData, err := script.BuildBootstrapScript(r.ServerPool.BootstrapScripts, immutable)
	if err != nil {
		return nil, nil, err
	}

	sshID, err := strconv.Atoi(immutable.ProviderConfig().SSH.Identifier)
	if err != nil {
		return nil, nil, err
	}

	var droplet *godo.Droplet
	for j := 0; j < expected.(*Droplet).Count; j++ {
		createRequest := &godo.DropletCreateRequest{
			Name:   fmt.Sprintf("%s-%d", expected.(*Droplet).Name, j),
			Region: expected.(*Droplet).Region,
			Size:   expected.(*Droplet).Size,
			Image: godo.DropletCreateImage{
				Slug: expected.(*Droplet).Image,
			},
			Tags:              []string{expected.(*Droplet).Name},
			PrivateNetworking: true,
			SSHKeys: []godo.DropletCreateSSHKey{
				{
					ID:          sshID,
					Fingerprint: expected.(*Droplet).SSHFingerprint,
				},
			},
			UserData: string(userData),
		}
		droplet, _, err = Sdk.Client.Droplets.Create(context.TODO(), createRequest)
		if err != nil {
			return nil, nil, err
		}
		logger.Success("Created Droplet [%d]", droplet.ID)
	}

	newResource := &Droplet{
		Shared: Shared{
			Name:    r.ServerPool.Name,
			CloudID: strconv.Itoa(droplet.ID),
		},
		Image:            droplet.Image.Slug,
		Size:             droplet.Size.Slug,
		Region:           droplet.Region.Slug,
		Count:            expected.(*Droplet).Count,
		BootstrapScripts: expected.(*Droplet).BootstrapScripts,
	}

	providerConfig = immutable.ProviderConfig()
	providerConfig.KubernetesAPI.Endpoint = masterIPPublic
	immutable.SetProviderConfig(providerConfig)

	newCluster := r.immutableRender(newResource, immutable)
	return newCluster, newResource, nil
}
func (r *Droplet) Delete(actual cloud.Resource, immutable *cluster.Cluster) (*cluster.Cluster, cloud.Resource, error) {
	logger.Debug("droplet.Delete")
	deleteResource := actual.(*Droplet)
	if deleteResource.Name == "" {
		return nil, nil, fmt.Errorf("Unable to delete droplet resource without Name [%s]", deleteResource.Name)
	}

	droplets, _, err := Sdk.Client.Droplets.ListByTag(context.TODO(), r.Name, &godo.ListOptions{})
	if err != nil {
		return nil, nil, err
	}
	if len(droplets) != actual.(*Droplet).Count {
		for i := 0; i < DeleteAttempts; i++ {
			logger.Info("Droplet count mis-match, trying query again")
			time.Sleep(5 * time.Second)
			droplets, _, err = Sdk.Client.Droplets.ListByTag(context.TODO(), r.Name, &godo.ListOptions{})
			if err != nil {
				return nil, nil, err
			}
			if len(droplets) == actual.(*Droplet).Count {
				break
			}
		}
	}

	for _, droplet := range droplets {
		for i := 0; i < DeleteAttempts; i++ {
			if droplet.Status == "new" {
				logger.Debug("Waiting for Droplet creation to finish [%d]...", droplet.ID)
				time.Sleep(DeleteSleepSecondsPerAttempt * time.Second)
			} else {
				break
			}
		}
		_, err = Sdk.Client.Droplets.Delete(context.TODO(), droplet.ID)
		if err != nil {
			return nil, nil, err
		}
		logger.Success("Deleted Droplet [%d]", droplet.ID)
	}

	// Kubernetes API
	// todo (@kris-nova) this is obviously not immutable
	immutable.ProviderConfig().KubernetesAPI.Endpoint = ""

	newResource := &Droplet{}
	newResource.Name = actual.(*Droplet).Name
	newResource.Tags = actual.(*Droplet).Tags
	newResource.Image = actual.(*Droplet).Image
	newResource.Size = actual.(*Droplet).Size
	newResource.Count = actual.(*Droplet).Count
	newResource.Region = actual.(*Droplet).Region
	newResource.BootstrapScripts = actual.(*Droplet).BootstrapScripts

	newCluster := r.immutableRender(newResource, immutable)
	return newCluster, newResource, nil
}

func (r *Droplet) immutableRender(newResource cloud.Resource, inaccurateCluster *cluster.Cluster) *cluster.Cluster {
	logger.Debug("droplet.Render")
	newCluster := inaccurateCluster
	serverPool := &cluster.ServerPool{}
	serverPool.Type = r.ServerPool.Type
	serverPool.Image = newResource.(*Droplet).Image
	serverPool.Size = newResource.(*Droplet).Size
	serverPool.Name = newResource.(*Droplet).Name
	serverPool.MaxCount = newResource.(*Droplet).Count
	serverPool.BootstrapScripts = newResource.(*Droplet).BootstrapScripts
	found := false

	machineProviderConfigs := newCluster.MachineProviderConfigs()
	for i := 0; i < len(machineProviderConfigs); i++ {
		machineProviderConfig := machineProviderConfigs[i]

		if machineProviderConfig.ServerPool.Name == newResource.(*Droplet).Name {
			machineProviderConfig.ServerPool.Image = newResource.(*Droplet).Image
			machineProviderConfig.ServerPool.Size = newResource.(*Droplet).Size
			machineProviderConfig.ServerPool.MaxCount = newResource.(*Droplet).Count
			machineProviderConfig.ServerPool.BootstrapScripts = newResource.(*Droplet).BootstrapScripts
			machineProviderConfig.ServerPool.Type = serverPool.Type
			found = true
			machineProviderConfigs[i] = machineProviderConfig
			newCluster.SetMachineProviderConfigs(machineProviderConfigs)
		}
	}
	if !found {
		providerConfig := []*cluster.MachineProviderConfig{
			{
				ServerPool: serverPool,
			},
		}
		newCluster.NewMachineSetsFromProviderConfigs(providerConfig)
	}
	providerConfig := newCluster.ProviderConfig()
	providerConfig.Location = newResource.(*Droplet).Region
	newCluster.SetProviderConfig(providerConfig)
	return newCluster
}

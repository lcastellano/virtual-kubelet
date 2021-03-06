// Copyright 2018 VMware, Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package vic

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"

	"gopkg.in/yaml.v2"

	"github.com/vmware/vic/pkg/trace"
)

type VicConfig struct {
	PersonaAddr   string `yaml:"persona-server"`
	PortlayerAddr string `yaml:"portlayer-server"`

}

const (
	personaAddrEnv   = "PERSONA_ADDR"
	portlayerAddrEnv = "PORTLAYER_ADDR"
)

func NewVicConfig(op trace.Operation, configFile string) VicConfig {
	var config VicConfig

	if configFile == "" {
		config.loadConfigFromEnv()
	} else {
		config.loadConfigFile(configFile)
	}

	return config
}

func (v *VicConfig) loadConfigFile(configFile string) error {
	op := trace.NewOperation(context.Background(), "LoadConfigFile - %s", configFile)
	defer trace.End(trace.Begin("", op))

	contents, err := ioutil.ReadFile(configFile)
	if err != nil {
		return err
	}

	var config VicConfig
	err = yaml.Unmarshal(contents, &config)
	if err != nil {
		err = fmt.Errorf("Unable to unmarshal vic virtual kubelet configfile: %s", err.Error())
		op.Error(err)
		return err
	}

	*v = config

	return nil
}

func (v *VicConfig) loadConfigFromEnv() {
	v.PersonaAddr = os.Getenv(personaAddrEnv)
	v.PortlayerAddr = os.Getenv(portlayerAddrEnv)
}

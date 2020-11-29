// Copyright 2020 The conprof Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package e2econprof

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/cortexproject/cortex/integration/e2e"
	"github.com/cortexproject/cortex/pkg/util"
	"github.com/pkg/errors"
)

const logLevel = "info"

// Same as default for now.
var defaultBackoffConfig = util.BackoffConfig{
	MinBackoff: 300 * time.Millisecond,
	MaxBackoff: 600 * time.Millisecond,
	MaxRetries: 50,
}

// DefaultImage returns the local docker image to use to run Thanos.
func DefaultImage() string {
	// Get the Thanos image from the THANOS_IMAGE env variable.
	if os.Getenv("CONPROF_IMAGE") != "" {
		return os.Getenv("CONPROF_IMAGE")
	}

	return "conprof"
}

func NewStorage(sharedDir string, networkName string, name string, dirSuffix string) (*Service, error) {
	dir := filepath.Join(sharedDir, "data", "storage", dirSuffix)
	dataDir := filepath.Join(dir, "data")
	container := filepath.Join(e2e.ContainerSharedDir, "data", "storage", dirSuffix)
	if err := os.MkdirAll(dataDir, 0777); err != nil {
		return nil, errors.Wrap(err, "create storage dir")
	}

	storage := NewService(
		fmt.Sprintf("storage-%v", name),
		DefaultImage(),
		e2e.NewCommand("storage", e2e.BuildArgs(map[string]string{
			"--debug.name":        fmt.Sprintf("storage-%v", name),
			"--grpc-address":      ":9091",
			"--grpc-grace-period": "0s",
			"--http-address":      ":8080",
			"--storage.tsdb.path": filepath.Join(container, "data"),
			"--log.level":         logLevel,
		})...),
		e2e.NewHTTPReadinessProbe(8080, "/-/ready", 200, 200),
		8080,
		9091,
	)
	storage.SetUser(strconv.Itoa(os.Getuid()))
	storage.SetBackoff(defaultBackoffConfig)

	return storage, nil
}
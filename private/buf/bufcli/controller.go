// Copyright 2020-2023 Buf Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package bufcli

import (
	"github.com/bufbuild/buf/private/bufnew/bufapi"
	"github.com/bufbuild/buf/private/bufnew/bufctl"
	"github.com/bufbuild/buf/private/bufnew/bufmodule/bufmoduleapi"
	"github.com/bufbuild/buf/private/pkg/app/appflag"
)

// NewController returns a new Controller.
func NewController(
	container appflag.Container,
	options ...bufctl.ControllerOption,
) (bufctl.Controller, error) {
	clientConfig, err := NewConnectClientConfig(container)
	if err != nil {
		return nil, err
	}
	clientProvider := bufapi.NewClientProvider(clientConfig)
	moduleDataProvider, err := newModuleDataProvider(container, clientProvider)
	if err != nil {
		return nil, err
	}
	return bufctl.NewController(
		container.Logger(),
		container,
		bufmoduleapi.NewModuleKeyProvider(container.Logger(), clientProvider),
		moduleDataProvider,
		// TODO: Delete defaultHTTPClient and use the one from newConfig
		defaultHTTPClient,
		defaultHTTPAuthenticator,
		defaultGitClonerOptions,
		options...,
	)
}
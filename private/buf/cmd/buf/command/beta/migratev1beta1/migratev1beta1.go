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

package migratev1beta1

import (
	"context"

	"github.com/bufbuild/buf/private/buf/bufmigrate"
	"github.com/bufbuild/buf/private/pkg/app"
	"github.com/bufbuild/buf/private/pkg/app/appcmd"
	"github.com/bufbuild/buf/private/pkg/app/appext"
	"github.com/spf13/pflag"
)

// NewCommand returns a new Command.
func NewCommand(
	name string,
	builder appext.SubCommandBuilder,
) *appcmd.Command {
	flags := newFlags()
	return &appcmd.Command{
		Use:   name + " <directory>",
		Short: `Migrate v1beta1 configuration to the latest version`,
		Long: `Migrate any v1beta1 configuration files in the directory to the latest version.
Defaults to the current directory if not specified.`,
		Args: appcmd.MaximumNArgs(1),
		Run: builder.NewRunFunc(
			func(ctx context.Context, container appext.Container) error {
				return run(ctx, container, flags)
			},
		),
		BindFlags: flags.Bind,
	}
}

type flags struct{}

func newFlags() *flags {
	return &flags{}
}

func (f *flags) Bind(flagSet *pflag.FlagSet) {}

func run(
	ctx context.Context,
	container appext.Container,
	flags *flags,
) error {
	dirPath, err := getDirPath(container)
	if err != nil {
		return err
	}
	return bufmigrate.NewV1Beta1Migrator(
		"buf config migrate-v1beta1",
		bufmigrate.V1Beta1MigratorWithNotifier(newWriteMessageFunc(container)),
	).Migrate(dirPath)
}

func getDirPath(container app.Container) (string, error) {
	switch numArgs := container.NumArgs(); numArgs {
	case 0:
		return ".", nil
	case 1:
		return container.Arg(0), nil
	default:
		return "", appcmd.NewInvalidArgumentErrorf("only 1 argument allowed but %d arguments specified", numArgs)
	}
}

func newWriteMessageFunc(container app.StderrContainer) func(string) error {
	return func(message string) error {
		_, err := container.Stderr().Write([]byte(message))
		return err
	}
}

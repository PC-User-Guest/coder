package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/cli/safeexec"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
	"golang.org/x/xerrors"

	"github.com/coder/coder/cli/cliflag"
	"github.com/coder/coder/cli/cliui"
	"github.com/coder/coder/coderd/database"
	"github.com/coder/coder/codersdk"
)

const sshStartToken = "# ------------START-CODER-----------"
const sshStartMessage = `# This was generated by "coder config-ssh".
#
# To remove this blob, run:
#
#    coder config-ssh --remove
#
# You should not hand-edit this section, unless you are deleting it.`
const sshEndToken = "# ------------END-CODER------------"

func configSSH() *cobra.Command {
	var (
		sshConfigFile    string
		sshOptions       []string
		skipProxyCommand bool
	)
	cmd := &cobra.Command{
		Use: "config-ssh",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := createClient(cmd)
			if err != nil {
				return err
			}
			organization, err := currentOrganization(cmd, client)
			if err != nil {
				return err
			}
			if strings.HasPrefix(sshConfigFile, "~/") {
				dirname, _ := os.UserHomeDir()
				sshConfigFile = filepath.Join(dirname, sshConfigFile[2:])
			}
			// Doesn't matter if this fails, because we write the file anyways.
			sshConfigContentRaw, _ := os.ReadFile(sshConfigFile)
			sshConfigContent := string(sshConfigContentRaw)
			startIndex := strings.Index(sshConfigContent, sshStartToken)
			endIndex := strings.Index(sshConfigContent, sshEndToken)
			if startIndex != -1 && endIndex != -1 {
				sshConfigContent = sshConfigContent[:startIndex-1] + sshConfigContent[endIndex+len(sshEndToken):]
			}

			workspaces, err := client.WorkspacesByOwner(cmd.Context(), organization.ID, codersdk.Me)
			if err != nil {
				return err
			}
			if len(workspaces) == 0 {
				return xerrors.New("You don't have any workspaces!")
			}

			binaryFile, err := currentBinPath(cmd)
			if err != nil {
				return err
			}

			root := createConfig(cmd)
			sshConfigContent += "\n" + sshStartToken + "\n" + sshStartMessage + "\n\n"
			sshConfigContentMutex := sync.Mutex{}
			var errGroup errgroup.Group
			for _, workspace := range workspaces {
				workspace := workspace
				errGroup.Go(func() error {
					resources, err := client.TemplateVersionResources(cmd.Context(), workspace.LatestBuild.TemplateVersionID)
					if err != nil {
						return err
					}
					for _, resource := range resources {
						if resource.Transition != database.WorkspaceTransitionStart {
							continue
						}
						for _, agent := range resource.Agents {
							sshConfigContentMutex.Lock()
							hostname := workspace.Name
							if len(resource.Agents) > 1 {
								hostname += "." + agent.Name
							}
							configOptions := []string{
								"Host coder." + hostname,
							}
							for _, option := range sshOptions {
								configOptions = append(configOptions, "\t"+option)
							}
							configOptions = append(configOptions,
								"\tHostName coder."+hostname,
								"\tConnectTimeout=0",
								"\tStrictHostKeyChecking=no",
								// Without this, the "REMOTE HOST IDENTITY CHANGED"
								// message will appear.
								"\tUserKnownHostsFile=/dev/null",
								// This disables the "Warning: Permanently added 'hostname' (RSA) to the list of known hosts."
								// message from appearing on every SSH. This happens because we ignore the known hosts.
								"\tLogLevel ERROR",
							)
							if !skipProxyCommand {
								configOptions = append(configOptions, fmt.Sprintf("\tProxyCommand %q --global-config %q ssh --stdio %s", binaryFile, root, hostname))
							}
							sshConfigContent += strings.Join(configOptions, "\n") + "\n"
							sshConfigContentMutex.Unlock()
						}
					}
					return nil
				})
			}
			err = errGroup.Wait()
			if err != nil {
				return err
			}
			sshConfigContent += "\n" + sshEndToken
			err = os.MkdirAll(filepath.Dir(sshConfigFile), os.ModePerm)
			if err != nil {
				return err
			}
			err = os.WriteFile(sshConfigFile, []byte(sshConfigContent), os.ModePerm)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "An auto-generated ssh config was written to %q\n", sshConfigFile)
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "You should now be able to ssh into your workspace")
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "For example, try running\n\n\t$ ssh coder.%s\n\n", workspaces[0].Name)
			return nil
		},
	}
	cliflag.StringVarP(cmd.Flags(), &sshConfigFile, "ssh-config-file", "", "CODER_SSH_CONFIG_FILE", "~/.ssh/config", "Specifies the path to an SSH config.")
	cmd.Flags().StringArrayVarP(&sshOptions, "ssh-option", "o", []string{}, "Specifies additional SSH options to embed in each host stanza.")
	cmd.Flags().BoolVarP(&skipProxyCommand, "skip-proxy-command", "", false, "Specifies whether the ProxyCommand option should be skipped. Useful for testing.")
	_ = cmd.Flags().MarkHidden("skip-proxy-command")

	return cmd
}

// currentBinPath returns the path to the coder binary suitable for use in ssh
// ProxyCommand.
func currentBinPath(cmd *cobra.Command) (string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return "", xerrors.Errorf("get executable path: %w", err)
	}

	binName := filepath.Base(exePath)
	// We use safeexec instead of os/exec because os/exec returns paths in
	// the current working directory, which we will run into very often when
	// looking for our own path.
	pathPath, err := safeexec.LookPath(binName)
	// On Windows, the coder-cli executable must be in $PATH for both Msys2/Git
	// Bash and OpenSSH for Windows (used by Powershell and VS Code) to function
	// correctly. Check if the current executable is in $PATH, and warn the user
	// if it isn't.
	if err != nil && runtime.GOOS == "windows" {
		cliui.Warn(cmd.OutOrStdout(),
			"The current executable is not in $PATH.",
			"This may lead to problems connecting to your workspace via SSH.",
			fmt.Sprintf("Please move %q to a location in your $PATH (such as System32) and run `%s config-ssh` again.", binName, binName),
		)
		// Return the exePath so SSH at least works outside of Msys2.
		return exePath, nil
	}

	// Warn the user if the current executable is not the same as the one in
	// $PATH.
	if filepath.Clean(pathPath) != filepath.Clean(exePath) {
		cliui.Warn(cmd.OutOrStdout(),
			"The current executable path does not match the executable path found in $PATH.",
			"This may cause issues connecting to your workspace via SSH.",
			fmt.Sprintf("\tCurrent executable path: %q", exePath),
			fmt.Sprintf("\tExecutable path in $PATH: %q", pathPath),
		)
	}

	return binName, nil
}

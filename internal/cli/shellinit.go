// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// shellInitShells lists the shells brig shell-init writes for.
var shellInitShells = []string{"bash", "zsh", "fish"}

// posixShellInit defines the brig function for bash and zsh. brig use prints
// nothing when it fails, so the shell keeps its variables then.
const posixShellInit = `# brig use NAME points the openshell CLI in this shell at VM NAME's gateway,
# brig use --unset at none; everything else runs brig.
brig() {
  if [ "${1-}" = use ]; then
    shift
    local __brig_code
    __brig_code="$(` + shellIntegrationEnv + `=sh command brig use "$@")" || return
    eval "$__brig_code"
  else
    command brig "$@"
  fi
}
`

// p10kSegment defines the brig segment of Powerlevel10k, which shows the VM
// whose gateway OPENSHELL_GATEWAY names. It reads only that variable, so it
// also works in instant prompt.
const p10kSegment = `
# Powerlevel10k: add brig to POWERLEVEL9K_LEFT_PROMPT_ELEMENTS or
# POWERLEVEL9K_RIGHT_PROMPT_ELEMENTS in ~/.p10k.zsh to show the VM that
# OPENSHELL_GATEWAY points at.
prompt_brig() {
  [[ $OPENSHELL_GATEWAY == brig-?* ]] || return
  p10k segment -f 208 -i brig -t "${OPENSHELL_GATEWAY#brig-}"
}
instant_prompt_brig() { prompt_brig; }
`

const fishShellInit = `# brig use NAME points the openshell CLI in this shell at VM NAME's gateway,
# brig use --unset at none; everything else runs brig.
function brig --description 'Manage agent-sandbox VMs running NVIDIA OpenShell'
    if test "$argv[1]" = use
        ` + shellIntegrationEnv + `=fish command brig $argv | source
        return $pipestatus[1]
    end
    command brig $argv
end
`

func newShellInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:       "shell-init SHELL",
		Short:     "Print the shell integration that provides brig use",
		ValidArgs: shellInitShells,
		Args:      cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		Long: `Print the shell integration for SHELL (bash, zsh or fish). It defines a
brig shell function, so that brig use NAME points the openshell CLI in the
current shell at VM NAME's gateway, as eval "$(brig env NAME)" does, and
brig use --unset at none. Other commands run brig as before.

Load it from your shell's startup file:

  bash   eval "$(brig shell-init bash)"     in ~/.bashrc
  zsh    eval "$(brig shell-init zsh)"      in ~/.zshrc
  fish   brig shell-init fish | source      in ~/.config/fish/config.fish

For zsh, it also defines a Powerlevel10k prompt segment named brig, which
shows the VM that OPENSHELL_GATEWAY points at. Add brig to
POWERLEVEL9K_LEFT_PROMPT_ELEMENTS or POWERLEVEL9K_RIGHT_PROMPT_ELEMENTS in
~/.p10k.zsh to show it, and style it with the usual POWERLEVEL9K_BRIG_*
parameters, such as POWERLEVEL9K_BRIG_FOREGROUND. Other prompts can show
${OPENSHELL_GATEWAY#brig-} when OPENSHELL_GATEWAY starts with brig-.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			switch args[0] {
			case "fish":
				fmt.Fprint(out, fishShellInit)
			case "zsh":
				fmt.Fprint(out, posixShellInit+p10kSegment)
			default:
				fmt.Fprint(out, posixShellInit)
			}
			return nil
		},
	}
}

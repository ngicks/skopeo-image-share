package commands

import (
	"fmt"

	"github.com/ngicks/skopeo-image-share/pkg/cli/skopeo"
	"github.com/ngicks/skopeo-image-share/pkg/imageref"
	"github.com/ngicks/skopeo-image-share/pkg/skopeoimageshare"
	"github.com/spf13/cobra"
)

var dumpCmd = &cobra.Command{
	Use:   "dump IMAGE [IMAGE...]",
	Short: "Dump local images into the on-disk OCI store layout.",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runDump,
}

var dumpFlags struct {
	localTransport string
	localPath      string
	localDumpDir   string
}

func init() {
	rootCmd.AddCommand(dumpCmd)

	f := dumpCmd.Flags()
	f.StringVar(&dumpFlags.localTransport, "local-transport", "containers-storage", "containers-storage|docker-daemon|oci")
	f.StringVar(&dumpFlags.localPath, "local-path", "", "local oci: dir (only when --local-transport=oci)")
	f.StringVar(&dumpFlags.localDumpDir, "local-dumpdir", "",
		"base of the local on-disk store layout; "+
			"when empty, falls back to ${XDG_DATA_HOME:-$HOME/.local/share}/skopeo-image-share")
}

func runDump(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	local, err := skopeoimageshare.NewLocal(ctx, skopeoimageshare.LocalConfig{
		BaseDir:   dumpFlags.localDumpDir,
		Transport: skopeo.Transport(dumpFlags.localTransport),
		OCIPath:   dumpFlags.localPath,
	})
	if err != nil {
		return err
	}

	var failed int
	for _, raw := range args {
		ref, err := imageref.Parse(raw)
		if err != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "%s ERROR: %v\n", raw, err)
			failed++
			continue
		}
		tagDir, err := local.Dump(ctx, ref)
		if err != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "%s ERROR: %v\n", ref.String(), err)
			failed++
			continue
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s -> %s\n", ref.String(), tagDir)
	}
	if failed > 0 {
		return fmt.Errorf("%d image(s) failed", failed)
	}
	return nil
}

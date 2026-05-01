package commands

import (
	"fmt"

	"github.com/ngicks/skopeo-image-share/pkg/cli/skopeo"
	"github.com/ngicks/skopeo-image-share/pkg/imageref"
	"github.com/ngicks/skopeo-image-share/pkg/skopeoimageshare"
	"github.com/spf13/cobra"
)

var dumpCmd = &cobra.Command{
	Use:   "dump",
	Short: "Dump local images into the on-disk OCI store layout.",
	Args:  cobra.ArbitraryArgs,
	RunE:  runDump,
}

var dumpFlags struct {
	images         []string
	localTransport string
	localPath      string
	dataDir        string
}

func init() {
	rootCmd.AddCommand(dumpCmd)

	f := dumpCmd.Flags()
	f.StringSliceVar(&dumpFlags.images, "image", nil, "image ref to dump (repeatable)")
	f.StringVar(&dumpFlags.localTransport, "local-transport", "containers-storage", "containers-storage|docker-daemon|oci")
	f.StringVar(&dumpFlags.localPath, "local-path", "", "local oci: dir (only when --local-transport=oci)")
	f.StringVar(&dumpFlags.dataDir, "data-dir", "", "override $XDG_DATA_HOME data dir")
}

func runDump(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	if len(dumpFlags.images) == 0 && len(args) == 0 {
		return fmt.Errorf("no images: use --image (repeatable) or positional args")
	}
	images := append([]string(nil), dumpFlags.images...)
	images = append(images, args...)

	local, err := skopeoimageshare.NewLocal(ctx, skopeoimageshare.LocalConfig{
		BaseDir:   dumpFlags.dataDir,
		Transport: skopeo.Transport(dumpFlags.localTransport),
		OCIPath:   dumpFlags.localPath,
	})
	if err != nil {
		return err
	}

	var failed int
	for _, raw := range images {
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

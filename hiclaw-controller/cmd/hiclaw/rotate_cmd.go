package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func rotateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rotate",
		Short: "Rotate credentials",
	}
	cmd.AddCommand(rotateAppServiceTokenCmd())
	return cmd
}

func rotateAppServiceTokenCmd() *cobra.Command {
	var asToken, hsToken string
	cmd := &cobra.Command{
		Use:   "appservice-token",
		Short: "Rotate the Matrix AppService as_token/hs_token",
		Long: `Rotates the Matrix Application Service token on the homeserver.

This command:
  1. Unregisters the current AppService registration (via admin command)
  2. Registers a new AppService with the provided token(s)
  3. Verifies the new token works via smoke test

After running this command, you MUST update HICLAW_MATRIX_APPSERVICE_AS_TOKEN
(and optionally HICLAW_MATRIX_APPSERVICE_HS_TOKEN) in your env file or K8s
Secret, then restart the controller for the change to take effect permanently.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			client := NewAPIClient()
			body := map[string]string{"as_token": asToken}
			if hsToken != "" {
				body["hs_token"] = hsToken
			}
			var resp map[string]string
			if err := client.DoJSON("POST", "/api/v1/appservice/rotate-token", body, &resp); err != nil {
				return fmt.Errorf("rotate appservice token: %w", err)
			}
			fmt.Println(resp["message"])
			fmt.Println()
			fmt.Println("Next steps:")
			fmt.Println("  1. Update HICLAW_MATRIX_APPSERVICE_AS_TOKEN in your env file or Secret")
			fmt.Println("  2. Restart the controller")
			return nil
		},
	}
	cmd.Flags().StringVar(&asToken, "as-token", "", "New AppService as_token (required)")
	cmd.Flags().StringVar(&hsToken, "hs-token", "", "New AppService hs_token (optional)")
	_ = cmd.MarkFlagRequired("as-token")
	return cmd
}

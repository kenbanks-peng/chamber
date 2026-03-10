package commands

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/cirruslabs/chamber/internal/executor"
	"github.com/cirruslabs/chamber/internal/ssh"
	"github.com/cirruslabs/chamber/internal/vm/tart"
	"github.com/spf13/cobra"
)

func NewVMCmd() *cobra.Command {
	var vmImage string

	cmd := &cobra.Command{
		Use:   "vm",
		Short: "Start a graphical VM with the current directory mounted and an SSH shell",
		Long: `Start an ephemeral Tart virtual machine in graphical mode with the current directory mounted.
Opens an interactive SSH shell to the VM without running any agent.

Example:
  chamber vm
  chamber vm --vm=macos-xcode`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVM(cmd.Context(), vmImage)
		},
	}

	cmd.Flags().StringVar(&vmImage, "vm", "chamber-seed", "Tart VM image to use (default: chamber-seed)")

	return cmd
}

func runVM(ctx context.Context, vmImage string) error {
	if !tart.Installed() {
		return fmt.Errorf("tart is not installed. Please install it from https://github.com/cirruslabs/tart")
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current directory: %w", err)
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return fmt.Errorf("failed to get absolute path: %w", err)
	}

	dirName := filepath.Base(cwd)

	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Fprintln(os.Stderr, "\nInterrupted, cleaning up...")
		cancel()
	}()

	fmt.Fprintf(os.Stdout, "Creating ephemeral VM from %s...\n", vmImage)
	vm, err := tart.NewVMClonedFrom(ctx, vmImage, nil)
	if err != nil {
		return err
	}
	defer func() {
		fmt.Fprintln(os.Stdout, "Cleaning up VM...")
		if err := vm.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to clean up VM: %v\n", err)
		}
	}()

	fmt.Fprintln(os.Stdout, "Configuring VM...")
	if err := vm.Configure(ctx, cpuCount, memoryMB); err != nil {
		return err
	}

	fmt.Fprintln(os.Stdout, "Starting VM in graphical mode...")
	directoryMounts := []tart.DirectoryMount{
		{
			Name:     dirName,
			Path:     cwd,
			ReadOnly: false,
		},
	}
	for _, dir := range extraDirs {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			return fmt.Errorf("failed to get absolute path for %q: %w", dir, err)
		}
		directoryMounts = append(directoryMounts, tart.DirectoryMount{
			Name:     filepath.Base(absDir),
			Path:     absDir,
			ReadOnly: false,
		})
	}
	vm.StartWithOptions(ctx, directoryMounts, true)

	fmt.Fprintln(os.Stdout, "Waiting for VM to boot...")
	ip, err := vm.RetrieveIP(ctx)
	if err != nil {
		return fmt.Errorf("failed to get VM IP: %w", err)
	}
	fmt.Fprintf(os.Stdout, "VM IP: %s\n", ip)

	select {
	case err := <-vm.ErrChan():
		if err != nil {
			return fmt.Errorf("VM failed to start: %w", err)
		}
	default:
	}

	fmt.Fprintln(os.Stdout, "Connecting to VM via SSH...")
	sshAddr := fmt.Sprintf("%s:22", ip)
	sshClient, err := ssh.WaitForSSH(ctx, sshAddr, sshUser, sshPass)
	if err != nil {
		return fmt.Errorf("failed to connect via SSH: %w", err)
	}
	defer sshClient.Close()

	exec := executor.New(sshClient, cwd, dirName)

	fmt.Fprintln(os.Stdout, "Mounting working directory...")
	if err := exec.MountWorkingDirectory(ctx); err != nil {
		return err
	}
	defer func() {
		_ = exec.UnmountWorkingDirectory(ctx)
	}()

	fmt.Fprintln(os.Stdout, "Opening interactive shell...")
	fmt.Fprintln(os.Stdout, strings.Repeat("-", 80))

	if err := exec.ExecuteInteractiveShell(ctx); err != nil {
		return err
	}

	return nil
}

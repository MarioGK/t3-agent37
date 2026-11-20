package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
)

type sandboxSize struct {
	CPU int `json:"cpu"`
	RAM int `json:"memory"`
}

var defaultSandboxSize = sandboxSize{CPU: 4, RAM: 8}

// Agent37 Default ties the vCPU count to the memory size.
var sandboxSizes = []sandboxSize{{2, 4}, defaultSandboxSize, {4, 16}, {4, 32}}

func (size sandboxSize) String() string {
	return fmt.Sprintf("%d vCPU · %d GB RAM", size.CPU, size.RAM)
}

func (values workstationConfig) sandboxSize() sandboxSize {
	if values.Resources == nil {
		return defaultSandboxSize
	}
	return *values.Resources
}

func selectSandboxSize(current sandboxSize, cpu, ram *int) (sandboxSize, error) {
	switch {
	case ram != nil:
		for _, size := range sandboxSizes {
			if size.RAM == *ram && (cpu == nil || size.CPU == *cpu) {
				return size, nil
			}
		}
	case cpu != nil:
		if current.CPU == *cpu && slices.Contains(sandboxSizes, current) {
			return current, nil
		}
		for _, size := range sandboxSizes {
			if size.CPU == *cpu {
				return size, nil
			}
		}
	default:
		if slices.Contains(sandboxSizes, current) {
			return current, nil
		}
	}
	return sandboxSize{}, errors.New("Agent37 Default sizes are 2 vCPU with 4 GB, or 4 vCPU with 8, 16, or 32 GB. Use --ram to select a size")
}

func configureSandboxSize(ctx context.Context, opts options, output io.Writer) error {
	file, err := readWorkstationConfig()
	if err != nil {
		return err
	}
	size, err := selectSandboxSize(file.values.sandboxSize(), opts.cpu, opts.ram)
	if err != nil {
		return err
	}
	title := "Default sandbox size"
	if !opts.showSize {
		file.values.Resources = &size
		if err := file.save(ctx); err != nil {
			return err
		}
		title = "Default sandbox size saved"
	}
	if _, terminal := ttyOutput(output); terminal {
		return printDetails(output, title, detail{"Resources", size.String()})
	}
	return jsonOutput(output, size, true)
}

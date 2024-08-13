package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/utils"
	bv "github.com/containernetworking/plugins/pkg/utils/buildversion"
	"github.com/ncode/cni-outbound/pkg/iptables"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type LogConfig struct {
	Enable bool   `json:"enable"`
	File   string `json:"file"`
}

type PluginConf struct {
	types.NetConf

	MainChainName string                  `json:"mainChainName"`
	DefaultAction string                  `json:"defaultAction"`
	OutboundRules []iptables.OutboundRule `json:"outboundRules"`
	Logging       LogConfig               `json:"logging"`
}

var logger *slog.Logger

func setupLogging(conf *PluginConf) error {
	if !conf.Logging.Enable {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		return nil
	}

	var logWriter *os.File
	if conf.Logging.File != "" {
		currentDate := time.Now().Format("2006-01-02")
		logFileName := fmt.Sprintf("%s-%s.log", strings.TrimSuffix(conf.Logging.File, filepath.Ext(conf.Logging.File)), currentDate)

		file, err := os.OpenFile(logFileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return fmt.Errorf("failed to open log file: %v", err)
		}
		logWriter = file
	} else {
		logWriter = os.Stderr
	}

	opts := slog.HandlerOptions{
		AddSource: true,
		Level:     slog.LevelInfo,
	}
	handler := slog.NewJSONHandler(logWriter, &opts)
	logger = slog.New(handler)
	return nil
}

func generateChainName(netName, containerID string) string {
	return utils.MustFormatChainNameWithPrefix(netName, containerID, "OUT-")
}

func parseAdditionalRules(args, containerID string) ([]iptables.OutboundRule, error) {
	logger.Log(context.Background(), slog.LevelInfo,
		"Parsing additional rules from args",
		slog.String("component", "CNI-Outbound"),
		slog.String("containerID", containerID),
		slog.String("details", args),
	)
	var additionalRules []iptables.OutboundRule

	if args == "" {
		logger.Log(context.Background(), slog.LevelInfo,
			"No additional args provided",
			slog.String("component", "CNI-Outbound"),
			slog.String("containerID", containerID),
		)
		return additionalRules, nil
	}

	kvs := strings.Split(args, ";")
	for _, kv := range kvs {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 || parts[0] != "outbound.additional_rules" {
			continue
		}

		logger.Log(context.Background(), slog.LevelInfo,
			"Found outbound.additional_rules",
			slog.String("component", "CNI-Outbound"),
			slog.String("containerID", containerID),
			slog.String("rules", parts[1]),
		)

		if err := json.Unmarshal([]byte(parts[1]), &additionalRules); err != nil {
			logger.Log(context.Background(), slog.LevelError,
				"Failed to parse additional rules",
				slog.String("component", "CNI-Outbound"),
				slog.String("containerID", containerID),
				slog.Any("error", err),
			)
			return nil, fmt.Errorf("failed to parse additional rules from CNI args: %v", err)
		}
		break
	}

	logger.Log(context.Background(), slog.LevelInfo,
		"Parsed additional rules",
		slog.String("component", "CNI-Outbound"),
		slog.String("containerID", containerID),
		slog.Int("ruleCount", len(additionalRules)),
	)
	return additionalRules, nil
}

func parseConfig(stdin []byte, args, containerID string) (*PluginConf, error) {
	conf := PluginConf{}

	if err := json.Unmarshal(stdin, &conf); err != nil {
		return nil, fmt.Errorf("failed to parse network configuration: %v", err)
	}

	if err := setupLogging(&conf); err != nil {
		return nil, fmt.Errorf("failed to setup logging: %v", err)
	}

	logger.Log(context.Background(), slog.LevelInfo,
		"Parsing configuration",
		slog.String("component", "CNI-Outbound"),
		slog.String("containerID", containerID),
	)

	if err := version.ParsePrevResult(&conf.NetConf); err != nil {
		logger.Log(context.Background(), slog.LevelError,
			"Could not parse prevResult",
			slog.String("component", "CNI-Outbound"),
			slog.String("containerID", containerID),
			slog.Any("error", err),
		)
		return nil, fmt.Errorf("could not parse prevResult: %v", err)
	}

	if conf.MainChainName == "" {
		logger.Log(context.Background(), slog.LevelInfo,
			"Using default MainChainName: CNI-OUTBOUND",
			slog.String("component", "CNI-Outbound"),
			slog.String("containerID", containerID),
		)
		conf.MainChainName = "CNI-OUTBOUND"
	}

	if conf.DefaultAction == "" {
		logger.Log(context.Background(), slog.LevelInfo,
			"Using default DefaultAction: DROP",
			slog.String("component", "CNI-Outbound"),
			slog.String("containerID", containerID),
		)
		conf.DefaultAction = "DROP"
	}

	logger.Log(context.Background(), slog.LevelInfo,
		"Base configuration",
		slog.String("component", "CNI-Outbound"),
		slog.String("containerID", containerID),
		slog.String("MainChainName", conf.MainChainName),
		slog.String("DefaultAction", conf.DefaultAction),
	)

	// Parse and append additional rules from CNI args, if any
	additionalRules, err := parseAdditionalRules(args, containerID)
	if err != nil {
		return nil, err
	}
	if len(additionalRules) > 0 {
		logger.Log(context.Background(), slog.LevelInfo,
			"Appending additional rules",
			slog.String("component", "CNI-Outbound"),
			slog.String("containerID", containerID),
			slog.Int("ruleCount", len(additionalRules)),
		)
		conf.OutboundRules = append(conf.OutboundRules, additionalRules...)
	}

	logger.Log(context.Background(), slog.LevelInfo,
		"Total outbound rules",
		slog.String("component", "CNI-Outbound"),
		slog.String("containerID", containerID),
		slog.Int("totalRules", len(conf.OutboundRules)),
	)
	return &conf, nil
}

func cmdAdd(args *skel.CmdArgs) error {
	conf, err := parseConfig(args.StdinData, args.Args, args.ContainerID)
	if err != nil {
		logger.Log(context.Background(), slog.LevelError,
			"Failed to parse config",
			slog.String("component", "CNI-Outbound"),
			slog.String("containerID", args.ContainerID),
			slog.Any("error", err),
		)
		return err
	}

	logger.Log(context.Background(), slog.LevelInfo,
		"CNI ADD called",
		slog.String("component", "CNI-Outbound"),
		slog.String("containerID", args.ContainerID),
	)

	logger.Log(context.Background(), slog.LevelInfo,
		"Creating IPTablesManager",
		slog.String("component", "CNI-Outbound"),
		slog.String("containerID", args.ContainerID),
	)

	iptManager, err := iptables.NewIPTablesManager(conf.MainChainName, conf.DefaultAction)
	if err != nil {
		logger.Log(context.Background(), slog.LevelError,
			"Failed to create IPTablesManager",
			slog.String("component", "CNI-Outbound"),
			slog.String("containerID", args.ContainerID),
			slog.Any("error", err),
		)
		return fmt.Errorf("failed to create IPTablesManager: %v", err)
	}

	logger.Log(context.Background(), slog.LevelInfo,
		"Ensuring main chain exists",
		slog.String("component", "CNI-Outbound"),
		slog.String("containerID", args.ContainerID),
	)

	if err := iptManager.EnsureMainChainExists(); err != nil {
		logger.Log(context.Background(), slog.LevelError,
			"Failed to ensure main chain exists",
			slog.String("component", "CNI-Outbound"),
			slog.String("containerID", args.ContainerID),
			slog.Any("error", err),
		)
		return fmt.Errorf("failed to ensure main chain exists: %v", err)
	}

	logger.Log(context.Background(), slog.LevelInfo,
		"Creating container chain",
		slog.String("component", "CNI-Outbound"),
		slog.String("containerID", args.ContainerID),
	)

	containerChain := generateChainName(conf.Name, args.ContainerID)
	if err := iptManager.CreateContainerChain(containerChain); err != nil {
		logger.Log(context.Background(), slog.LevelError,
			"Failed to create container chain",
			slog.String("component", "CNI-Outbound"),
			slog.String("containerID", args.ContainerID),
			slog.Any("error", err),
		)
		return fmt.Errorf("failed to create container chain: %v", err)
	}

	logger.Log(context.Background(), slog.LevelInfo,
		"Adding rules to container chain",
		slog.String("component", "CNI-Outbound"),
		slog.String("containerID", args.ContainerID),
	)

	for _, rule := range conf.OutboundRules {
		logger.Log(context.Background(), slog.LevelInfo,
			"Adding rule",
			slog.String("component", "CNI-Outbound"),
			slog.String("containerID", args.ContainerID),
			slog.Any("rule", rule),
		)
		if err := iptManager.AddRule(containerChain, rule); err != nil {
			logger.Log(context.Background(), slog.LevelError,
				"Failed to add rule to container chain",
				slog.String("component", "CNI-Outbound"),
				slog.String("containerID", args.ContainerID),
				slog.Any("error", err),
				slog.Any("rule", rule),
			)
			return fmt.Errorf("failed to add rule to container chain: %v", err)
		}
	}

	logger.Log(context.Background(), slog.LevelInfo,
		"Adding jump rule to main chain",
		slog.String("component", "CNI-Outbound"),
		slog.String("containerID", args.ContainerID),
	)

	result, err := current.GetResult(conf.PrevResult)
	if err != nil {
		logger.Log(context.Background(), slog.LevelError,
			"Failed to get result from prevResult",
			slog.String("component", "CNI-Outbound"),
			slog.String("containerID", args.ContainerID),
			slog.Any("error", err),
		)
		return fmt.Errorf("failed to convert prevResult: %v", err)
	}

	if len(result.IPs) == 0 {
		logger.Log(context.Background(), slog.LevelError,
			"No IPs found in the result",
			slog.String("component", "CNI-Outbound"),
			slog.String("containerID", args.ContainerID),
		)
		return fmt.Errorf("got no container IPs")
	}

	containerIP := result.IPs[0].Address.IP.String()
	logger.Log(context.Background(), slog.LevelInfo,
		"Container IP obtained",
		slog.String("component", "CNI-Outbound"),
		slog.String("containerID", args.ContainerID),
		slog.String("ip", containerIP),
	)
	if err := iptManager.AddJumpRule(containerIP, containerChain); err != nil {
		logger.Log(context.Background(), slog.LevelError,
			"Failed to add jump rule to main chain",
			slog.String("component", "CNI-Outbound"),
			slog.String("containerID", args.ContainerID),
			slog.Any("error", err),
		)
		return fmt.Errorf("failed to add jump rule to main chain: %v", err)
	}

	logger.Log(context.Background(), slog.LevelInfo,
		"CNI ADD completed successfully",
		slog.String("component", "CNI-Outbound"),
		slog.String("containerID", args.ContainerID),
	)
	return types.PrintResult(result, conf.CNIVersion)
}

func cmdDel(args *skel.CmdArgs) error {
	conf, err := parseConfig(args.StdinData, args.Args, args.ContainerID)
	if err != nil {
		logger.Log(context.Background(), slog.LevelError,
			"Failed to parse config",
			slog.String("component", "CNI-Outbound"),
			slog.String("containerID", args.ContainerID),
			slog.Any("error", err),
		)
		return err
	}

	logger.Log(context.Background(), slog.LevelInfo,
		"CNI DEL called",
		slog.String("component", "CNI-Outbound"),
		slog.String("containerID", args.ContainerID),
	)

	logger.Log(context.Background(), slog.LevelInfo,
		"Creating IPTablesManager",
		slog.String("component", "CNI-Outbound"),
		slog.String("containerID", args.ContainerID),
	)

	iptManager, err := iptables.NewIPTablesManager(conf.MainChainName, conf.DefaultAction)
	if err != nil {
		logger.Log(context.Background(), slog.LevelError,
			"Failed to create IPTablesManager",
			slog.String("component", "CNI-Outbound"),
			slog.String("containerID", args.ContainerID),
			slog.Any("error", err),
		)
		return fmt.Errorf("failed to create IPTablesManager: %v", err)
	}

	logger.Log(context.Background(), slog.LevelInfo,
		"Removing container chain",
		slog.String("component", "CNI-Outbound"),
		slog.String("containerID", args.ContainerID),
	)

	containerChain := generateChainName(conf.Name, args.ContainerID)
	if err := iptManager.RemoveJumpRuleByTargetChain(containerChain); err != nil {
		logger.Log(context.Background(), slog.LevelWarn,
			"Failed to remove jump rule from main chain",
			slog.String("component", "CNI-Outbound"),
			slog.String("containerID", args.ContainerID),
			slog.Any("error", err),
		)
	}

	logger.Log(context.Background(), slog.LevelInfo,
		"Clearing and deleting container chain",
		slog.String("component", "CNI-Outbound"),
		slog.String("containerID", args.ContainerID),
	)

	if err := iptManager.ClearAndDeleteChain(containerChain); err != nil {
		logger.Log(context.Background(), slog.LevelError,
			"Failed to clear and delete container chain",
			slog.String("component", "CNI-Outbound"),
			slog.String("containerID", args.ContainerID),
			slog.Any("error", err),
		)
		return fmt.Errorf("failed to clear and delete container chain: %v", err)
	}

	logger.Log(context.Background(), slog.LevelInfo,
		"CNI DEL completed successfully",
		slog.String("component", "CNI-Outbound"),
		slog.String("containerID", args.ContainerID),
	)
	return nil
}

func cmdCheck(args *skel.CmdArgs) error {
	conf, err := parseConfig(args.StdinData, args.Args, args.ContainerID)
	if err != nil {
		logger.Log(context.Background(), slog.LevelError,
			"Failed to parse config",
			slog.String("component", "CNI-Outbound"),
			slog.String("containerID", args.ContainerID),
			slog.Any("error", err),
		)
		return err
	}

	logger.Log(context.Background(), slog.LevelInfo,
		"CNI CHECK called",
		slog.String("component", "CNI-Outbound"),
		slog.String("containerID", args.ContainerID),
	)

	logger.Log(context.Background(), slog.LevelInfo,
		"Creating IPTablesManager",
		slog.String("component", "CNI-Outbound"),
		slog.String("containerID", args.ContainerID),
	)

	iptManager, err := iptables.NewIPTablesManager(conf.MainChainName, conf.DefaultAction)
	if err != nil {
		logger.Log(context.Background(), slog.LevelError,
			"Failed to create IPTablesManager",
			slog.String("component", "CNI-Outbound"),
			slog.String("containerID", args.ContainerID),
			slog.Any("error", err),
		)
		return fmt.Errorf("failed to create IPTablesManager: %v", err)
	}

	logger.Log(context.Background(), slog.LevelInfo,
		"Checking if main chain exists",
		slog.String("component", "CNI-Outbound"),
		slog.String("containerID", args.ContainerID),
	)

	exists, err := iptManager.ChainExists(conf.MainChainName)
	if err != nil {
		logger.Log(context.Background(), slog.LevelError,
			"Failed to check if main chain exists",
			slog.String("component", "CNI-Outbound"),
			slog.String("containerID", args.ContainerID),
			slog.Any("error", err),
		)
		return fmt.Errorf("failed to check if main chain exists: %v", err)
	}
	if !exists {
		logger.Log(context.Background(), slog.LevelError,
			"Main chain does not exist",
			slog.String("component", "CNI-Outbound"),
			slog.String("containerID", args.ContainerID),
			slog.String("chain", conf.MainChainName),
		)
		return fmt.Errorf("main chain %s does not exist", conf.MainChainName)
	}

	logger.Log(context.Background(), slog.LevelInfo,
		"Checking container chain",
		slog.String("component", "CNI-Outbound"),
		slog.String("containerID", args.ContainerID),
	)

	containerChain := generateChainName(conf.Name, args.ContainerID)
	exists, err = iptManager.ChainExists(containerChain)
	if err != nil {
		logger.Log(context.Background(), slog.LevelError,
			"Failed to check if container chain exists",
			slog.String("component", "CNI-Outbound"),
			slog.String("containerID", args.ContainerID),
			slog.Any("error", err),
		)
		return fmt.Errorf("failed to check if container chain exists: %v", err)
	}
	if !exists {
		logger.Log(context.Background(), slog.LevelError,
			"Container chain does not exist",
			slog.String("component", "CNI-Outbound"),
			slog.String("containerID", args.ContainerID),
			slog.String("chain", containerChain),
		)
		return fmt.Errorf("container chain %s does not exist", containerChain)
	}

	logger.Log(context.Background(), slog.LevelInfo,
		"Verifying rules in container chain",
		slog.String("component", "CNI-Outbound"),
		slog.String("containerID", args.ContainerID),
	)

	if err := iptManager.VerifyRules(containerChain, conf.OutboundRules); err != nil {
		logger.Log(context.Background(), slog.LevelError,
			"Rule verification failed",
			slog.String("component", "CNI-Outbound"),
			slog.String("containerID", args.ContainerID),
			slog.Any("error", err),
		)
		return fmt.Errorf("rule verification failed: %v", err)
	}

	logger.Log(context.Background(), slog.LevelInfo,
		"CNI CHECK completed successfully",
		slog.String("component", "CNI-Outbound"),
		slog.String("containerID", args.ContainerID),
	)
	return nil
}

func main() {
	skel.PluginMain(cmdAdd, cmdCheck, cmdDel, version.All, bv.BuildString("outbound"))
}

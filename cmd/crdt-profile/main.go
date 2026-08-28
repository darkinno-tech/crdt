// Command crdt-profile lists the library's curated CRDT selection profiles.
// It only describes protocol choices; it never creates a network service,
// reads application secrets, or selects production resource limits.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/darkinno-tech/crdt"
)

type profileOutput struct {
	ID               string          `json:"id"`
	Title            string          `json:"title"`
	Summary          string          `json:"summary"`
	ConflictRule     string          `json:"conflict_rule"`
	RecommendedFor   []string        `json:"recommended_for"`
	NotFor           []string        `json:"not_for"`
	HostRequirements []string        `json:"host_requirements"`
	RequiresCodecID  bool            `json:"requires_codec_id"`
	FrameType        frameTypeOutput `json:"frame_type"`
}

type frameTypeOutput struct {
	StateID          uint64 `json:"state_id"`
	DeltaID          uint64 `json:"delta_id"`
	SemanticsVersion uint64 `json:"semantics_version"`
	UsesHLC          bool   `json:"uses_hlc"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("crdt-profile", flag.ContinueOnError)
	flags.SetOutput(stderr)
	id := flags.String("id", "", "exact stable profile ID; omit to list every profile")
	format := flags.String("format", "text", "text or json")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "crdt-profile accepts flags only")
		return 2
	}

	profiles, err := selectProfiles(*id)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	switch *format {
	case "text":
		if err := writeText(stdout, profiles); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
	case "json":
		if err := json.NewEncoder(stdout).Encode(toOutput(profiles)); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
	default:
		_, _ = fmt.Fprintln(stderr, "-format must be text or json")
		return 2
	}
	return 0
}

func selectProfiles(id string) ([]crdt.ReplicationProfile, error) {
	if id == "" {
		return crdt.ReplicationProfiles(), nil
	}
	profile, ok := crdt.ReplicationProfileFor(id)
	if !ok {
		return nil, fmt.Errorf("unknown CRDT profile %q; use -format=text without -id to list supported profiles", id)
	}
	return []crdt.ReplicationProfile{profile}, nil
}

func toOutput(profiles []crdt.ReplicationProfile) []profileOutput {
	result := make([]profileOutput, len(profiles))
	for index, profile := range profiles {
		result[index] = profileOutput{
			ID:               profile.ID,
			Title:            profile.Title,
			Summary:          profile.Summary,
			ConflictRule:     profile.ConflictRule,
			RecommendedFor:   profile.RecommendedFor,
			NotFor:           profile.NotFor,
			HostRequirements: profile.HostRequirements,
			RequiresCodecID:  profile.RequiresCodecID,
			FrameType: frameTypeOutput{
				StateID:          profile.FrameType.StateID,
				DeltaID:          profile.FrameType.DeltaID,
				SemanticsVersion: profile.FrameType.SemanticsVersion,
				UsesHLC:          profile.FrameType.UsesHLC,
			},
		}
	}
	return result
}

func writeText(output io.Writer, profiles []crdt.ReplicationProfile) error {
	for index, profile := range profiles {
		if index > 0 {
			if _, err := fmt.Fprintln(output); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(output, "%s — %s\n%s\nconflict: %s\nrecommended: %s\nnot for: %s\nhost must: %s\nframe: state=%d delta=%d semantics=%d uses_hlc=%t requires_codec_id=%t\n", profile.ID, profile.Title, profile.Summary, profile.ConflictRule, strings.Join(profile.RecommendedFor, "; "), strings.Join(profile.NotFor, "; "), strings.Join(profile.HostRequirements, "; "), profile.FrameType.StateID, profile.FrameType.DeltaID, profile.FrameType.SemanticsVersion, profile.FrameType.UsesHLC, profile.RequiresCodecID); err != nil {
			return err
		}
	}
	return nil
}

package server

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const DefaultAddress = ":8999"

var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{32,512}$`)

type Config struct {
	Address                       string
	TLSDir                        string
	JoinToken                     string
	InviteHost                    string
	InviteRoom                    string
	PrintInvite                   bool
	ShowVersion                   bool
	MaxConnections                int
	MaxConnectionsPerIP           int
	MaxRooms                      int
	MaxRoomParticipants           int
	StreamingEnabled              bool
	StreamingDERPMapURL           string
	MaxStreamOffersPerParticipant int
	MaxStreamViewersPerOffer      int
	StreamGrantTTL                time.Duration
}

func DefaultConfig() Config {
	return Config{
		Address:        DefaultAddress,
		TLSDir:         defaultTLSDirectory(),
		JoinToken:      os.Getenv("FARO_JOIN_TOKEN"),
		InviteRoom:     "watch",
		MaxConnections: 1024, MaxConnectionsPerIP: 32,
		MaxRooms: 1000, MaxRoomParticipants: 100,
		StreamingEnabled:              true,
		MaxStreamOffersPerParticipant: 4, MaxStreamViewersPerOffer: 5,
		StreamGrantTTL: 4 * time.Hour,
	}
}

func ParseConfig(args []string, output io.Writer) (Config, error) {
	cfg := DefaultConfig()
	flags := flag.NewFlagSet("faro-server", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&cfg.Address, "listen", cfg.Address, "TLS listen address")
	flags.StringVar(&cfg.TLSDir, "tls-dir", cfg.TLSDir, "directory containing or receiving the TLS identity")
	flags.StringVar(&cfg.JoinToken, "join-token", cfg.JoinToken, "optional high-entropy server join token")
	flags.StringVar(&cfg.InviteHost, "invite-host", "", "public host name used when printing an invite")
	flags.StringVar(&cfg.InviteRoom, "invite-room", cfg.InviteRoom, "room used when printing an invite")
	flags.BoolVar(&cfg.PrintInvite, "print-invite", false, "print a faro:// invite before serving")
	flags.BoolVar(&cfg.ShowVersion, "version", false, "print version and exit")
	flags.IntVar(&cfg.MaxConnections, "max-connections", cfg.MaxConnections, "maximum concurrent client connections")
	flags.IntVar(&cfg.MaxConnectionsPerIP, "max-connections-per-ip", cfg.MaxConnectionsPerIP, "maximum concurrent connections from one IP")
	flags.IntVar(&cfg.MaxRooms, "max-rooms", cfg.MaxRooms, "maximum active rooms")
	flags.IntVar(&cfg.MaxRoomParticipants, "max-room-participants", cfg.MaxRoomParticipants, "maximum participants in one room")
	flags.BoolVar(&cfg.StreamingEnabled, "streaming", cfg.StreamingEnabled, "enable direct peer-to-peer media streaming")
	flags.StringVar(&cfg.StreamingDERPMapURL, "streaming-derp-map-url", "", "optional custom Tailcat bootstrap DERP map URL")
	flags.IntVar(&cfg.MaxStreamOffersPerParticipant, "max-stream-offers-per-participant", cfg.MaxStreamOffersPerParticipant, "maximum active media offers per participant")
	flags.IntVar(&cfg.MaxStreamViewersPerOffer, "max-stream-viewers-per-offer", cfg.MaxStreamViewersPerOffer, "maximum viewers admitted to one media offer")
	flags.DurationVar(&cfg.StreamGrantTTL, "stream-grant-ttl", cfg.StreamGrantTTL, "maximum lifetime of one media stream grant")
	flags.Lookup("join-token").DefValue = ""
	if err := flags.Parse(args); err != nil {
		return Config{}, err
	}
	if flags.NArg() != 0 {
		return Config{}, fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.Address) == "" {
		return errors.New("listen address is required")
	}
	if _, port, err := net.SplitHostPort(c.Address); err != nil || port == "" {
		return fmt.Errorf("invalid listen address %q", c.Address)
	}
	if strings.TrimSpace(c.TLSDir) == "" {
		return errors.New("TLS directory is required")
	}
	if c.JoinToken != "" && !tokenPattern.MatchString(c.JoinToken) {
		return errors.New("join token must be 32-512 URL-safe characters")
	}
	if c.PrintInvite && strings.TrimSpace(c.InviteHost) == "" {
		return errors.New("--print-invite requires --invite-host")
	}
	if c.PrintInvite && strings.TrimSpace(c.InviteRoom) == "" {
		return errors.New("--invite-room is required when printing an invite")
	}
	if c.MaxConnections < 1 || c.MaxConnections > 100000 {
		return errors.New("max connections must be between 1 and 100000")
	}
	if c.MaxConnectionsPerIP < 1 || c.MaxConnectionsPerIP > c.MaxConnections {
		return errors.New("max connections per IP must be between 1 and max connections")
	}
	if c.MaxRooms < 1 || c.MaxRooms > 100000 {
		return errors.New("max rooms must be between 1 and 100000")
	}
	if c.MaxRoomParticipants < 1 || c.MaxRoomParticipants > 10000 {
		return errors.New("max room participants must be between 1 and 10000")
	}
	if c.MaxStreamOffersPerParticipant < 1 || c.MaxStreamOffersPerParticipant > 100 {
		return errors.New("max stream offers per participant must be between 1 and 100")
	}
	if c.MaxStreamViewersPerOffer < 1 || c.MaxStreamViewersPerOffer > 10000 {
		return errors.New("max stream viewers per offer must be between 1 and 10000")
	}
	if c.StreamingEnabled && c.MaxStreamViewersPerOffer > c.MaxRoomParticipants {
		return errors.New("max stream viewers per offer must not exceed max room participants")
	}
	if c.StreamingDERPMapURL != "" {
		parsed, err := url.Parse(c.StreamingDERPMapURL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
			return errors.New("streaming DERP map URL must be an absolute HTTPS URL")
		}
	}
	if c.StreamGrantTTL < time.Minute || c.StreamGrantTTL > 24*time.Hour {
		return errors.New("stream grant TTL must be between 1 minute and 24 hours")
	}
	return nil
}

func defaultTLSDirectory() string {
	if configured := os.Getenv("FARO_TLS_DIR"); configured != "" {
		return configured
	}
	root, err := os.UserConfigDir()
	if err != nil {
		root = "."
	}
	return filepath.Join(root, "faro", "tls")
}

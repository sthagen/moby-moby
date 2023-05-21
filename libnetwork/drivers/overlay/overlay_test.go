//go:build linux

package overlay

import (
	"testing"

	"github.com/docker/docker/libnetwork/driverapi"
	"github.com/docker/docker/pkg/plugingetter"
	"github.com/docker/libkv/store/boltdb"
)

func init() {
	boltdb.Register()
}

type driverTester struct {
	t *testing.T
	d *driver
}

const testNetworkType = "overlay"

func (dt *driverTester) GetPluginGetter() plugingetter.PluginGetter {
	return nil
}

func (dt *driverTester) RegisterDriver(name string, drv driverapi.Driver,
	cap driverapi.Capability) error {
	if name != testNetworkType {
		dt.t.Fatalf("Expected driver register name to be %q. Instead got %q",
			testNetworkType, name)
	}

	if _, ok := drv.(*driver); !ok {
		dt.t.Fatalf("Expected driver type to be %T. Instead got %T",
			&driver{}, drv)
	}

	dt.d = drv.(*driver)
	return nil
}

func TestOverlayInit(t *testing.T) {
	if err := Register(&driverTester{t: t}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestOverlayType(t *testing.T) {
	dt := &driverTester{t: t}
	if err := Register(dt, nil); err != nil {
		t.Fatal(err)
	}

	if dt.d.Type() != testNetworkType {
		t.Fatalf("Expected Type() to return %q. Instead got %q", testNetworkType,
			dt.d.Type())
	}
}

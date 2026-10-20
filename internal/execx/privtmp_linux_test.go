package execx

import "testing"

func TestPrivateTmp(t *testing.T) {
	// /proc/<DumpCat>/mountinfo of the dumpcat.service unit (PrivateTmp=true, systemd 249)
	const svc = `570 1 252:1 / / rw,relatime shared:1 - ext4 /dev/vda1 rw
616 570 252:1 /tmp/systemd-private-3fac8c230f97-dumpcat.service-jog530/tmp /tmp rw,relatime shared:301 master:1 - ext4 /dev/vda1 rw
619 570 252:1 /var/tmp/systemd-private-3fac8c230f97-dumpcat.service-aFWxae/tmp /var/tmp rw,relatime shared:304 master:1 - ext4 /dev/vda1 rw
`
	for dir, want := range map[string]string{"/var/tmp/pgr": "/var/tmp", "/tmp/a/b/": "/tmp", "/var/tmp": "/var/tmp", "/srv/pgr": "", "/var/tmpx/a": "", "/var/lib/dumpcat": ""} {
		if got := privateTmpIn(svc, dir); got != want {
			t.Errorf("%s: %q, want %q", dir, got, want)
		}
	}
	// a tmpfs on /tmp is shared by every process
	if got := privateTmpIn("30 1 0:26 / /tmp rw,nosuid,nodev shared:13 - tmpfs tmpfs rw\n", "/tmp/x"); got != "" {
		t.Errorf("tmpfs /tmp: %q", got)
	}
}

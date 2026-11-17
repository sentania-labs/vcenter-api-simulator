package admin

import (
	"os"
	"syscall"
	"testing"
)

func failWrites(t *testing.T, errno syscall.Errno) {
	t.Helper()
	original := writeFile
	writeFile = func(*os.File, []byte) (int, error) {
		return 0, &os.PathError{Op: "write", Path: "console-password", Err: errno}
	}
	t.Cleanup(func() { writeFile = original })
}

func TestDeploymentPasswordSurvivesFullVolume(t *testing.T) {
	const password = "deployment-password"
	t.Run("first claim", func(t *testing.T) {
		failWrites(t, syscall.ENOSPC)
		a, err := newConsoleAuth(t.TempDir(), password)
		if err != nil {
			t.Fatalf("startup on a full volume: %v", err)
		}
		if ok, err := a.claimed(); err != nil || !ok {
			t.Fatalf("claimed=%v err=%v", ok, err)
		}
		if _, err := a.login(password); err != nil {
			t.Fatalf("login with the deployment password: %v", err)
		}
		if _, err := a.login("another-password"); err == nil {
			t.Fatal("a wrong password signed in")
		}
	})
	t.Run("rotated password", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := newConsoleAuth(dir, "previous-password"); err != nil {
			t.Fatal(err)
		}
		failWrites(t, syscall.ENOSPC)
		a, err := newConsoleAuth(dir, password)
		if err != nil {
			t.Fatalf("startup on a full volume: %v", err)
		}
		if _, err := a.login(password); err != nil {
			t.Fatalf("login with the rotated password: %v", err)
		}
		if _, err := a.login("previous-password"); err == nil {
			t.Fatal("the replaced password still signed in")
		}
	})
	t.Run("other write errors still fail startup", func(t *testing.T) {
		failWrites(t, syscall.EIO)
		if _, err := newConsoleAuth(t.TempDir(), password); err == nil {
			t.Fatal("startup succeeded despite an unrelated write error")
		}
	})
}

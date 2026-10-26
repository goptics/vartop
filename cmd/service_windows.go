package cmd

import "os"

// taskService is the login service on Windows: the scheduled task "vartop
// daemon", run at the user's logon.
type taskService struct {
	run runner
}

func newSupervisor(run runner) supervisor { return &taskService{run: run} }

func (s *taskService) Installed() bool {
	return s.run("schtasks", "/Query", "/TN", serviceTask) == nil
}

// Install registers the task from its XML (replacing the one registered)
// and runs it.
func (s *taskService) Install(exe string, env map[string]string) error {
	f, err := os.CreateTemp("", "vartop-task-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(utf16File(windowsTaskXML(exe, env)))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := s.run("schtasks", "/Create", "/XML", f.Name(), "/TN", serviceTask, "/F"); err != nil {
		return err
	}
	return s.run("schtasks", "/Run", "/TN", serviceTask)
}

func (s *taskService) Uninstall() error {
	_ = s.run("schtasks", "/End", "/TN", serviceTask) // fails when it is not running
	return s.run("schtasks", "/Delete", "/TN", serviceTask, "/F")
}

// Stop ends the task's run, if any. It is no error when it is not
// running.
func (s *taskService) Stop() error {
	_ = s.run("schtasks", "/End", "/TN", serviceTask) // fails when it is not running
	return nil
}

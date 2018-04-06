package context

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"os/exec"
	"strings"
	"time"

	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/setting"
)

func InitialClone(cloneURL string, privateKeyPEM string, fullRepoDirPath string) error {
	fullRepoDirPath = strings.TrimRight(fullRepoDirPath, "/")
	log.GitLogger.Info("InitialClone: %#v -> %#v", cloneURL, fullRepoDirPath)

	gitcmd := exec.Command(
		"git",
		"clone",
		"--bare",
		cloneURL,
		fullRepoDirPath,
	)

	errorBuff := bytes.NewBuffer(nil)

	gitcmd.Dir = setting.RepoRootPath
	gitcmd.Stderr = errorBuff
	gitcmd.Stdout = os.Stdout
	gitcmd.Stdin = os.Stdin

	GIT_SSH_COMMAND := "ssh -o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=no"
	if privateKeyPEM != "" {
		pkeyPath := fullRepoDirPath + fmt.Sprintf("-%d-%d", time.Now().UnixNano(), rand.Int63()) + ".pem"
		defer os.Remove(pkeyPath)
		err := ioutil.WriteFile(pkeyPath, []byte(privateKeyPEM), os.FileMode(0400))
		if err != nil {
			return err
		}
		GIT_SSH_COMMAND += " -i " + pkeyPath
	}
	log.GitLogger.Info("GIT_SSH_COMMAND = %#v", GIT_SSH_COMMAND)
	gitcmd.Env = append(gitcmd.Env, "GIT_SSH_COMMAND="+GIT_SSH_COMMAND)

	defer log.GitLogger.Flush()

	err := gitcmd.Run()
	if err != nil {
		stderrText := errorBuff.String()
		if stderrText != "" {
			err = fmt.Errorf("%s: %s", err, stderrText)
		}
		return err
	}

	return nil
}

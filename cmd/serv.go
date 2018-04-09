// Copyright 2014 The Gogs Authors. All rights reserved.
// Copyright 2016 The Gitea Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"code.gitea.io/gitea/models"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/private"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/util"

	"github.com/Unknwon/com"
	"github.com/dgrijalva/jwt-go"
	"github.com/urfave/cli"
)

const (
	accessDenied        = "Repository does not exist or you do not have access"
	lfsAuthenticateVerb = "git-lfs-authenticate"
)

// CmdServ represents the available serv sub-command.
var CmdServ = cli.Command{
	Name:        "serv",
	Usage:       "This command should only be called by SSH shell",
	Description: `Serv provide access auth for repositories`,
	Action:      runServ,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "config, c",
			Value: "custom/conf/app.ini",
			Usage: "Custom configuration file path",
		},
	},
}

func setup(logPath string) error {
	setting.NewContext()
	log.NewGitLogger(filepath.Join(setting.LogRootPath, logPath))
	models.LoadConfigs()

	if setting.UseSQLite3 || setting.UseTiDB {
		workPath := setting.AppWorkPath
		if err := os.Chdir(workPath); err != nil {
			log.GitLogger.Fatal(4, "Failed to change directory %s: %v", workPath, err)
		}
	}

	setting.NewXORMLogService(true)
	return models.SetEngine()
}

func parseCmd(cmd string) (string, string) {
	ss := strings.SplitN(cmd, " ", 2)
	if len(ss) != 2 {
		return "", ""
	}
	return ss[0], strings.Replace(ss[1], "'/", "'", 1)
}

var (
	allowedCommands = map[string]models.AccessMode{
		"git-upload-pack":    models.AccessModeRead,
		"git-upload-archive": models.AccessModeRead,
		"git-receive-pack":   models.AccessModeWrite,
		lfsAuthenticateVerb:  models.AccessModeNone,
	}
)

func fail(userMessage, logMessage string, args ...interface{}) {
	fmt.Fprintln(os.Stderr, "Gitea:", userMessage)

	if len(logMessage) > 0 {
		if !setting.ProdMode {
			fmt.Fprintf(os.Stderr, logMessage+"\n", args...)
		}
		log.GitLogger.Fatal(3, logMessage, args...)
		return
	}

	log.GitLogger.Close()
	os.Exit(1)
}

func runServ(c *cli.Context) error {
	if c.IsSet("config") {
		setting.CustomConf = c.String("config")
	}

	if err := setup("serv.log"); err != nil {
		fail("System init failed", fmt.Sprintf("setup: %v", err))
	}

	if setting.SSH.Disabled {
		println("Gitea: SSH has been disabled")
		return nil
	}

	if len(c.Args()) < 1 {
		cli.ShowSubcommandHelp(c)
		return nil
	}

	cmd := os.Getenv("SSH_ORIGINAL_COMMAND")
	if len(cmd) == 0 {
		println("Hi there, You've successfully authenticated, but Gitea does not provide shell access.")
		println("If this is unexpected, please log in with password and setup Gitea under another user.")
		return nil
	}

	verb, args := parseCmd(cmd)

	var lfsVerb string
	if verb == lfsAuthenticateVerb {
		if !setting.LFS.StartServer {
			fail("Unknown git command", "LFS authentication request over SSH denied, LFS support is disabled")
		}

		argsSplit := strings.Split(args, " ")
		if len(argsSplit) >= 2 {
			args = strings.TrimSpace(argsSplit[0])
			lfsVerb = strings.TrimSpace(argsSplit[1])
		}
	}

	repoPath := strings.ToLower(strings.Trim(args, "'"))
	rr := strings.SplitN(repoPath, "/", 2)
	if len(rr) != 2 {
		fail("Invalid repository path", "Invalid repository path: %v", args)
	}

	username := strings.ToLower(rr[0])
	reponame := strings.ToLower(strings.TrimSuffix(rr[1], ".git"))

	isWiki := false
	unitType := models.UnitTypeCode
	if strings.HasSuffix(reponame, ".wiki") {
		isWiki = true
		unitType = models.UnitTypeWiki
		reponame = reponame[:len(reponame)-5]
	}

	env := map[string]string{}

	env[models.EnvRepoUsername] = username
	if isWiki {
		env[models.EnvRepoIsWiki] = "true"
	} else {
		env[models.EnvRepoIsWiki] = "false"
	}
	env[models.EnvRepoName] = reponame

	repo, err := models.GetRepositoryByOwnerAndName(username, reponame)
	if err != nil {
		if models.IsErrRepoNotExist(err) {
			fail(accessDenied, "Repository does not exist: %s/%s", username, reponame)
		}
		fail("Internal error", "Failed to get repository: %v", err)
	}

	requestedMode, has := allowedCommands[verb]
	if !has {
		fail("Unknown git command", "Unknown git command %s", verb)
	}

	if verb == lfsAuthenticateVerb {
		if lfsVerb == "upload" {
			requestedMode = models.AccessModeWrite
		} else if lfsVerb == "download" {
			requestedMode = models.AccessModeRead
		} else {
			fail("Unknown LFS verb", "Unknown lfs verb %s", lfsVerb)
		}
	}

	// Prohibit push to mirror repositories.
	if requestedMode > models.AccessModeRead && repo.IsMirror {
		fail("mirror repository is read-only", "")
	}

	// Allow anonymous clone for public repositories.
	var (
		keyID int64
		user  *models.User
	)
	if requestedMode == models.AccessModeWrite || repo.IsPrivate {
		keys := strings.Split(c.Args()[0], "-")
		if len(keys) != 2 {
			fail("Key ID format error", "Invalid key argument: %s", c.Args()[0])
		}

		key, err := models.GetPublicKeyByID(com.StrTo(keys[1]).MustInt64())
		if err != nil {
			fail("Invalid key ID", "Invalid key ID[%s]: %v", c.Args()[0], err)
		}
		keyID = key.ID

		// Check deploy key or user key.
		if key.Type == models.KeyTypeDeploy {
			if key.Mode < requestedMode {
				fail("Key permission denied", "Cannot push with deployment key: %d", key.ID)
			}
			// Check if this deploy key belongs to current repository.
			if !models.HasDeployKey(key.ID, repo.ID) {
				fail("Key access denied", "Deploy key access denied: [key_id: %d, repo_id: %d]", key.ID, repo.ID)
			}

			// Update deploy key activity.
			deployKey, err := models.GetDeployKeyByRepo(key.ID, repo.ID)
			if err != nil {
				fail("Internal error", "GetDeployKey: %v", err)
			}

			deployKey.UpdatedUnix = util.TimeStampNow()
			if err = models.UpdateDeployKeyCols(deployKey, "updated_unix"); err != nil {
				fail("Internal error", "UpdateDeployKey: %v", err)
			}
		} else {
			user, err = models.GetUserByKeyID(key.ID)
			if err != nil {
				fail("internal error", "Failed to get user by key ID(%d): %v", keyID, err)
			}

			mode, err := models.AccessLevel(user.ID, repo)
			if err != nil {
				fail("Internal error", "Failed to check access: %v", err)
			} else if mode < requestedMode {
				clientMessage := accessDenied
				if mode >= models.AccessModeRead {
					clientMessage = "You do not have sufficient authorization for this action"
				}
				fail(clientMessage,
					"User %s does not have level %v access to repository %s",
					user.Name, requestedMode, repoPath)
			}

			if !repo.CheckUnitUser(user.ID, user.IsAdmin, unitType) {
				fail("You do not have allowed for this action",
					"User %s does not have allowed access to repository %s 's code",
					user.Name, repoPath)
			}

			env[models.EnvPusherName] = user.Name
			env[models.EnvPusherID] = fmt.Sprintf("%d", user.ID)
		}
	}

	//LFS token authentication
	if verb == lfsAuthenticateVerb {
		url := fmt.Sprintf("%s%s/%s.git/info/lfs", setting.AppURL, username, repo.Name)

		now := time.Now()
		claims := jwt.MapClaims{
			"repo": repo.ID,
			"op":   lfsVerb,
			"exp":  now.Add(5 * time.Minute).Unix(),
			"nbf":  now.Unix(),
		}
		if user != nil {
			claims["user"] = user.ID
		}
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)

		// Sign and get the complete encoded token as a string using the secret
		tokenString, err := token.SignedString(setting.LFS.JWTSecretBytes)
		if err != nil {
			fail("Internal error", "Failed to sign JWT token: %v", err)
		}

		tokenAuthentication := &models.LFSTokenResponse{
			Header: make(map[string]string),
			Href:   url,
		}
		tokenAuthentication.Header["Authorization"] = fmt.Sprintf("Bearer %s", tokenString)

		enc := json.NewEncoder(os.Stdout)
		err = enc.Encode(tokenAuthentication)
		if err != nil {
			fail("Internal error", "Failed to encode LFS json response: %v", err)
		}

		return nil
	}

	// Special handle for Windows.
	if setting.IsWindows {
		verb = strings.Replace(verb, "-", " ", 1)
	}

	var gitcmd *exec.Cmd
	verbs := strings.Split(verb, " ")
	if len(verbs) == 2 {
		gitcmd = exec.Command(verbs[0], verbs[1], repoPath)
	} else {
		gitcmd = exec.Command(verb, repoPath)
	}

	if isWiki {
		if err = repo.InitWiki(); err != nil {
			fail("Internal error", "Failed to init wiki repo: %v", err)
		}
	}

	env[models.ProtectedBranchRepoID] = fmt.Sprintf("%d", repo.ID)

	for key, value := range env {
		gitcmd.Env = append(gitcmd.Env, fmt.Sprintf("%s=%s", key, value))
	}

	gitcmd.Dir = setting.RepoRootPath
	gitcmd.Stdout = os.Stdout
	gitcmd.Stdin = os.Stdin

	errorBuff := bytes.NewBuffer(nil)
	gitcmd.Stderr = errorBuff

	if err = gitcmd.Run(); err != nil {
		stderrText := errorBuff.String()
		if stderrText != "" {
			err = fmt.Errorf("%s: %s", err, stderrText)
		}
		fail("Internal error", "Failed to execute git command: %v", err)
	}

	// Update user key activity.
	if keyID > 0 {
		if err = private.UpdatePublicKeyUpdated(keyID); err != nil {
			fail("Internal error", "UpdatePublicKey: %v", err)
		}
	}

	return nil
}

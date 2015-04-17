package tools

import (
	"log"
	"os/exec"
)

func execCommand(cmd string) error {
	log.Printf("Exec cmd: %s \n", cmd)
	output, err := exec.Command("sh", "-c", cmd).CombinedOutput()
	if err != nil {
		log.Println(err.Error())
		log.Println("Output:\n" + string(output[:]))
	}
	return err
}

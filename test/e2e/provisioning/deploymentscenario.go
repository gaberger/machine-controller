/*
Copyright 2019 The Machine Controller Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package provisioning

import (
	"context"
	"fmt"
	"time"

	"github.com/golang/glog"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clusterv1alpha1 "sigs.k8s.io/cluster-api/pkg/apis/cluster/v1alpha1"
)

func verifyCreateUpdateAndDelete(kubeConfig, manifestPath string, parameters []string, timeout time.Duration) error {

	client, machineDeployment, err := prepareMachineDeployment(kubeConfig, manifestPath, parameters)
	if err != nil {
		return err
	}
	// This test inherently relies on replicas being one so we enforce that
	machineDeployment.Spec.Replicas = getInt32Ptr(1)

	machineDeployment, err = createAndAssure(machineDeployment, client, timeout)
	if err != nil {
		return fmt.Errorf("failed to verify creation of node for MachineDeployment: %v", err)
	}

	if err := updateMachineDeployment(machineDeployment, client, func(md *clusterv1alpha1.MachineDeployment) {
		md.Spec.Template.Labels["testUpdate"] = "true"
	}); err != nil {
		return fmt.Errorf("failed to update MachineDeployment %s after modifying it: %v", machineDeployment.Name, err)
	}

	glog.Infof("Waiting for second MachineSet to appear after updating MachineDeployment %s", machineDeployment.Name)
	var machineSets []clusterv1alpha1.MachineSet
	if err := wait.Poll(5*time.Second, timeout, func() (bool, error) {
		machineSets, err = getMachingMachineSets(machineDeployment, client)
		if err != nil {
			return false, err
		}
		if len(machineSets) != 2 {
			return false, err
		}
		for _, machineSet := range machineSets {
			if *machineSet.Spec.Replicas != int32(1) {
				return false, nil
			}
		}
		return true, nil
	}); err != nil {
		return err
	}
	glog.Infof("Found second MachineSet for MachineDeployment %s!", machineDeployment.Name)

	glog.Infof("Waiting for new MachineSets node to appear")
	var newestMachineSet, oldMachineSet clusterv1alpha1.MachineSet
	if machineSets[0].CreationTimestamp.Before(&machineSets[1].CreationTimestamp) {
		newestMachineSet = machineSets[1]
		oldMachineSet = machineSets[0]
	} else {
		newestMachineSet = machineSets[0]
		oldMachineSet = machineSets[1]
	}
	var machines []clusterv1alpha1.Machine
	if err := wait.Poll(5*time.Second, timeout, func() (bool, error) {
		machines, err = getMatchingMachinesForMachineset(&newestMachineSet, client)
		if err != nil {
			return false, err
		}
		if len(machines) != 1 {
			return false, nil
		}
		return true, nil
	}); err != nil {
		return err
	}
	glog.Infof("New MachineSet %s appeared with %v machines", newestMachineSet.Name, len(machines))

	glog.Infof("Waiting for new MachineSet %s to get a ready node", newestMachineSet.Name)
	if err := wait.Poll(5*time.Second, timeout, func() (bool, error) {
		return hasMachineReadyNode(&machines[0], client)
	}); err != nil {
		return err
	}
	glog.Infof("Found ready node for MachineSet %s", newestMachineSet.Name)

	glog.Infof("Waiting for old MachineSet %s to be scaled down and have no associated machines",
		oldMachineSet.Name)
	if err := wait.Poll(5*time.Second, timeout, func() (bool, error) {
		machineSet := &clusterv1alpha1.MachineSet{}
		if err := client.Get(context.Background(), types.NamespacedName{Namespace: oldMachineSet.Namespace, Name: oldMachineSet.Name}, machineSet); err != nil {
			return false, err
		}
		if *machineSet.Spec.Replicas != int32(0) {
			return false, nil
		}
		machines, err := getMatchingMachinesForMachineset(machineSet, client)
		if err != nil {
			return false, err
		}
		return len(machines) == 0, nil
	}); err != nil {
		return err
	}
	glog.Infof("Old MachineSet %s got scaled down and has no associated machines anymore", oldMachineSet.Name)

	glog.Infof("Setting replicas of MachineDeployment %s to 0 and waiting until it has no associated machines", machineDeployment.Name)
	if err := updateMachineDeployment(machineDeployment, client, func(md *clusterv1alpha1.MachineDeployment) {
		md.Spec.Replicas = getInt32Ptr(0)
	}); err != nil {
		return fmt.Errorf("failed to update replicas of MachineDeployment %s: %v", machineDeployment.Name, err)
	}
	glog.Infof("Successfully set replicas of MachineDeployment %s to 0", machineDeployment.Name)

	glog.Infof("Waiting for MachineDeployment %s to not have any associated machines", machineDeployment.Name)
	if err := wait.Poll(5*time.Second, timeout, func() (bool, error) {
		machines, err := getMatchingMachines(machineDeployment, client)
		return len(machines) == 0, err
	}); err != nil {
		return err
	}
	glog.Infof("Successfully waited for MachineDeployment %s to not have any associated machines", machineDeployment.Name)

	glog.Infof("Deleting MachineDeployment %s and waiting for it to disappear", machineDeployment.Name)
	if err := client.Delete(context.Background(), machineDeployment); err != nil {
		return fmt.Errorf("failed to delete MachineDeployment %s: %v", machineDeployment.Name, err)
	}
	if err := wait.Poll(5*time.Second, timeout, func() (bool, error) {
		err := client.Get(context.Background(), types.NamespacedName{Namespace: machineDeployment.Namespace, Name: machineDeployment.Name}, &clusterv1alpha1.MachineDeployment{})
		if kerrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}); err != nil {
		return err
	}
	glog.Infof("Successfully deleted MachineDeployment %s!", machineDeployment.Name)
	return nil
}

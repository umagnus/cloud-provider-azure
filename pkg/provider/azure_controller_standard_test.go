/*
Copyright 2020 The Kubernetes Authors.

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

package provider

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachineclient/mock_virtualmachineclient"
	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
)

var (
	fakeCacheTTL = 2 * time.Second
)

func TestStandardAttachDisk(t *testing.T) {

	testCases := []struct {
		desc            string
		nodeName        types.NodeName
		inconsistentLUN bool
		isAttachFail    bool
		expectedErr     bool
	}{
		{
			desc:        "an error shall be returned if there's no corresponding vms",
			nodeName:    "vm2",
			expectedErr: true,
		},
		{
			desc:        "no error shall be returned if everything's good",
			nodeName:    "vm1",
			expectedErr: false,
		},
		{
			desc:        "no error shall be returned if everything's good with non managed disk",
			nodeName:    "vm1",
			expectedErr: false,
		},
		{
			desc:            "an error shall be returned if update attach disk failed",
			nodeName:        "vm1",
			inconsistentLUN: true,
			expectedErr:     true,
		},
		{
			desc:        "error should be returned when disk lun is inconsistent",
			nodeName:    "vm1",
			expectedErr: false,
		},
	}

	for i, test := range testCases {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		ctx, cancel := getContextWithCancel()
		defer cancel()
		testCloud := GetTestCloud(ctrl)
		vmSet := testCloud.VMSet
		expectedVMs := setTestVirtualMachines(testCloud, map[string]string{"vm1": "PowerState/Running"}, false)
		mockVMsClient := testCloud.ComputeClientFactory.GetVirtualMachineClient().(*mock_virtualmachineclient.MockInterface)
		for _, vm := range expectedVMs {
			vm.Properties.StorageProfile = &armcompute.StorageProfile{
				OSDisk: &armcompute.OSDisk{
					Name: ptr.To("OSDisk1"),
					ManagedDisk: &armcompute.ManagedDiskParameters{
						ID: ptr.To("ManagedID"),
						DiskEncryptionSet: &armcompute.DiskEncryptionSetParameters{
							ID: ptr.To("DiskEncryptionSetID"),
						},
					},
				},
				DataDisks: []*armcompute.DataDisk{},
			}
			if test.inconsistentLUN {
				diskName := "disk-name2"
				diSKURI := "uri"
				vm.Properties.StorageProfile.DataDisks = []*armcompute.DataDisk{
					{Lun: ptr.To(int32(0)), Name: &diskName, ManagedDisk: &armcompute.ManagedDiskParameters{ID: &diSKURI}},
				}
			}
			mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, *vm.Name, gomock.Any()).Return(vm, nil).AnyTimes()
		}
		mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, "vm2", gomock.Any()).Return(&armcompute.VirtualMachine{}, &azcore.ResponseError{StatusCode: http.StatusNotFound, ErrorCode: cloudprovider.InstanceNotFound.Error()}).AnyTimes()
		if test.isAttachFail {
			mockVMsClient.EXPECT().CreateOrUpdate(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any()).Return(nil, &azcore.ResponseError{StatusCode: http.StatusNotFound, ErrorCode: cloudprovider.InstanceNotFound.Error()}).AnyTimes()
		} else {
			mockVMsClient.EXPECT().CreateOrUpdate(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
		}

		options := AttachDiskOptions{
			Lun:                     0,
			DiskName:                "disk-name2",
			CachingMode:             armcompute.CachingTypesReadOnly,
			DiskEncryptionSetID:     "",
			WriteAcceleratorEnabled: false,
		}
		if test.inconsistentLUN {
			options.Lun = 63
		}
		diskMap := map[string]*AttachDiskOptions{
			"uri": &options,
		}
		err := vmSet.AttachDisk(ctx, test.nodeName, diskMap)
		assert.Equal(t, test.expectedErr, err != nil, "TestCase[%d]: %s, err: %v", i, test.desc, err)
	}
}

func TestStandardDetachDisk(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := getContextWithCancel()
	defer cancel()

	testCases := []struct {
		desc          string
		nodeName      types.NodeName
		disks         []string
		isDetachFail  bool
		forceDetach   bool
		expectedError bool
	}{
		{
			desc:          "no error shall be returned if there's no corresponding vm",
			nodeName:      "vm2",
			expectedError: false,
		},
		{
			desc:          "no error shall be returned if there's no corresponding disk",
			nodeName:      "vm1",
			disks:         []string{"diskx"},
			expectedError: false,
		},
		{
			desc:          "no error shall be returned if there's a corresponding disk",
			nodeName:      "vm1",
			disks:         []string{"disk1"},
			expectedError: false,
		},
		{
			desc:          "no error shall be returned if there's 2 corresponding disks",
			nodeName:      "vm1",
			disks:         []string{"disk1", "disk2"},
			expectedError: false,
		},
		{
			desc:          "an error shall be returned if detach disk failed",
			nodeName:      "vm1",
			isDetachFail:  true,
			expectedError: true,
		},
		{
			desc:          "no error shall be returned if there's a corresponding disk with forceDetach",
			nodeName:      "vm1",
			disks:         []string{"disk1"},
			forceDetach:   true,
			expectedError: false,
		},
	}

	for i, test := range testCases {
		testCloud := GetTestCloud(ctrl)
		vmSet := testCloud.VMSet
		expectedVMs := setTestVirtualMachines(testCloud, map[string]string{"vm1": "PowerState/Running"}, false)
		mockVMsClient := testCloud.ComputeClientFactory.GetVirtualMachineClient().(*mock_virtualmachineclient.MockInterface)
		for _, vm := range expectedVMs {
			mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, *vm.Name, gomock.Any()).Return(vm, nil).AnyTimes()
		}
		mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, "vm2", gomock.Any()).Return(&armcompute.VirtualMachine{}, &azcore.ResponseError{StatusCode: http.StatusNotFound, ErrorCode: cloudprovider.InstanceNotFound.Error()}).AnyTimes()
		if test.isDetachFail {
			mockVMsClient.EXPECT().CreateOrUpdate(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any()).Return(nil, &azcore.ResponseError{StatusCode: http.StatusNotFound, ErrorCode: cloudprovider.InstanceNotFound.Error()}).AnyTimes()
		} else {
			mockVMsClient.EXPECT().CreateOrUpdate(gomock.Any(), testCloud.ResourceGroup, "vm1", gomock.Any()).Return(nil, nil).AnyTimes()
		}

		diskMap := map[string]string{}
		for _, diskName := range test.disks {
			diSKURI := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/disks/%s",
				testCloud.SubscriptionID, testCloud.ResourceGroup, diskName)
			diskMap[diSKURI] = diskName
		}
		err := vmSet.DetachDisk(ctx, test.nodeName, diskMap, test.forceDetach)
		assert.Equal(t, test.expectedError, err != nil, "TestCase[%d]: %s", i, test.desc)
		if !test.expectedError && len(test.disks) > 0 {
			dataDisks, _, err := vmSet.GetDataDisks(context.TODO(), test.nodeName, azcache.CacheReadTypeDefault)
			assert.Equal(t, 3, len(dataDisks), "TestCase[%d]: %s, err: %v", i, test.desc, err)
		}
	}
}

func TestStandardUpdateVM(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := getContextWithCancel()
	defer cancel()

	testCases := []struct {
		desc          string
		nodeName      types.NodeName
		diskName      string
		isDetachFail  bool
		expectedError bool
	}{
		{
			desc:          "no error shall be returned if there's no corresponding vm",
			nodeName:      "vm2",
			expectedError: false,
		},
		{
			desc:          "no error shall be returned if there's no corresponding disk",
			nodeName:      "vm1",
			diskName:      "disk2",
			expectedError: false,
		},
		{
			desc:          "no error shall be returned if there's a corresponding disk",
			nodeName:      "vm1",
			diskName:      "disk1",
			expectedError: false,
		},
		{
			desc:          "an error shall be returned if detach disk failed",
			nodeName:      "vm1",
			isDetachFail:  true,
			expectedError: true,
		},
	}

	for i, test := range testCases {
		testCloud := GetTestCloud(ctrl)
		vmSet := testCloud.VMSet
		expectedVMs := setTestVirtualMachines(testCloud, map[string]string{"vm1": "PowerState/Running"}, false)
		mockVMsClient := testCloud.ComputeClientFactory.GetVirtualMachineClient().(*mock_virtualmachineclient.MockInterface)
		for _, vm := range expectedVMs {
			mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, *vm.Name, gomock.Any()).Return(vm, nil).AnyTimes()
		}
		mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, "vm2", gomock.Any()).Return(&armcompute.VirtualMachine{}, runtime.NewResponseErrorWithErrorCode(&http.Response{StatusCode: http.StatusNotFound}, cloudprovider.InstanceNotFound.Error())).AnyTimes()

		if test.isDetachFail {
			mockVMsClient.EXPECT().CreateOrUpdate(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any()).Return(nil, &azcore.ResponseError{StatusCode: http.StatusBadRequest, ErrorCode: cloudprovider.InstanceNotFound.Error()}).AnyTimes()

		} else {
			mockVMsClient.EXPECT().CreateOrUpdate(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any()).Return(nil, &azcore.ResponseError{StatusCode: http.StatusNotFound, ErrorCode: cloudprovider.InstanceNotFound.Error()}).AnyTimes()
		}
		err := vmSet.UpdateVM(ctx, test.nodeName)
		assert.Equal(t, test.expectedError, err != nil, "TestCase[%d]: %s", i, test.desc)
		if !test.expectedError && test.diskName != "" {
			dataDisks, _, err := vmSet.GetDataDisks(context.TODO(), test.nodeName, azcache.CacheReadTypeDefault)
			assert.Equal(t, true, len(dataDisks) == 3, "TestCase[%d]: %s, err: %v", i, test.desc, err)
		}
	}
}

func TestGetDataDisks(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	var testCases = []struct {
		desc              string
		nodeName          types.NodeName
		crt               azcache.AzureCacheReadType
		isDataDiskNull    bool
		expectedError     bool
		expectedDataDisks []*armcompute.DataDisk
	}{
		{
			desc:              "an error shall be returned if there's no corresponding vm",
			nodeName:          "vm2",
			expectedDataDisks: nil,
			expectedError:     true,
			crt:               azcache.CacheReadTypeDefault,
		},
		{
			desc:     "correct list of data disks shall be returned if everything is good",
			nodeName: "vm1",
			expectedDataDisks: []*armcompute.DataDisk{
				{
					Lun:  ptr.To(int32(0)),
					Name: ptr.To("disk1"),
				},
				{
					Lun:  ptr.To(int32(1)),
					Name: ptr.To("disk2"),
				},
				{
					Lun:  ptr.To(int32(2)),
					Name: ptr.To("disk3"),
				},
			},
			expectedError: false,
			crt:           azcache.CacheReadTypeDefault,
		},
		{
			desc:     "correct list of data disks shall be returned if everything is good",
			nodeName: "vm1",
			expectedDataDisks: []*armcompute.DataDisk{
				{
					Lun:  ptr.To(int32(0)),
					Name: ptr.To("disk1"),
				},
				{
					Lun:  ptr.To(int32(1)),
					Name: ptr.To("disk2"),
				},
				{
					Lun:  ptr.To(int32(2)),
					Name: ptr.To("disk3"),
				},
			},
			expectedError: false,
			crt:           azcache.CacheReadTypeUnsafe,
		},
		{
			desc:              "nil shall be returned if DataDisk is null",
			nodeName:          "vm1",
			isDataDiskNull:    true,
			expectedDataDisks: nil,
			expectedError:     false,
			crt:               azcache.CacheReadTypeDefault,
		},
	}
	for i, test := range testCases {
		testCloud := GetTestCloud(ctrl)
		vmSet := testCloud.VMSet
		expectedVMs := setTestVirtualMachines(testCloud, map[string]string{"vm1": "PowerState/Running"}, false)
		mockVMsClient := testCloud.ComputeClientFactory.GetVirtualMachineClient().(*mock_virtualmachineclient.MockInterface)
		for _, vm := range expectedVMs {
			if test.isDataDiskNull {
				vm.Properties.StorageProfile = &armcompute.StorageProfile{}
			}
			mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, *vm.Name, gomock.Any()).Return(vm, nil).AnyTimes()
		}
		mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, gomock.Not("vm1"), gomock.Any()).Return(&armcompute.VirtualMachine{}, &azcore.ResponseError{StatusCode: http.StatusNotFound, ErrorCode: cloudprovider.InstanceNotFound.Error()}).AnyTimes()
		mockVMsClient.EXPECT().BeginUpdate(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

		dataDisks, _, err := vmSet.GetDataDisks(context.TODO(), test.nodeName, test.crt)
		assert.Equal(t, test.expectedDataDisks, dataDisks, "TestCase[%d]: %s", i, test.desc)
		assert.Equal(t, test.expectedError, err != nil, "TestCase[%d]: %s", i, test.desc)

		if test.crt == azcache.CacheReadTypeUnsafe {
			time.Sleep(fakeCacheTTL)
			dataDisks, _, err := vmSet.GetDataDisks(context.TODO(), test.nodeName, test.crt)
			assert.Equal(t, test.expectedDataDisks, dataDisks, "TestCase[%d]: %s", i, test.desc)
			assert.Equal(t, test.expectedError, err != nil, "TestCase[%d]: %s", i, test.desc)
		}
	}
}

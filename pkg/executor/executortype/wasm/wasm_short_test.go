package wasm

import (
	"context"
	"fmt"

	// "fmt"
	"testing"
	"time"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
	// "github.com/fission/fission/pkg/executor/util"
	fClient "github.com/fission/fission/pkg/generated/clientset/versioned/fake"
	genInformer "github.com/fission/fission/pkg/generated/informers/externalversions"
	"github.com/fission/fission/pkg/utils"
	"github.com/fission/fission/pkg/utils/loggerfactory"
	uuid "github.com/satori/go.uuid"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8sCache "k8s.io/client-go/tools/cache"
)



func TestShortFnCreate(t *testing.T){
	logger := loggerfactory.GetLogger()
	kubernetesClient := fake.NewSimpleClientset()
    fissionClient := fClient.NewSimpleClientset()
	informerFactory := genInformer.NewSharedInformerFactory(fissionClient, time.Minute*30)
	funcInformer := informerFactory.Core().V1().Functions()
	// envInformer := informerFactory.Core().V1().Environments()

	wasmInformerFactory, err := utils.GetInformerFactoryByExecutor(kubernetesClient, fv1.ExecutorTypeWasm, time.Minute*30)
	if err != nil {
		t.Fatalf("Error creating informer factory: %s", err)
	}

	deployInformer := wasmInformerFactory.Apps().V1().Deployments()
	svcInformer := wasmInformerFactory.Core().V1().Services()
	Jobnformer := wasmInformerFactory.Batch().V1().Jobs()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

    //创建wasm组件
	executor, err := MakeWasm(ctx, logger, fissionClient, kubernetesClient, functionNamespace,"test",funcInformer, Jobnformer,deployInformer,svcInformer)
	if err != nil {
		t.Fatalf("new deploy manager creation failed: %s", err)
	}

	wasm := executor.(*Wasm)

    //运行组件
	go wasm.Run(ctx)
	t.Log("New deploy manager started")

	// runInformers(ctx, []k8sCache.SharedIndexInformer{
	// 	funcInformer.Informer(),
	// 	deployInformer.Informer(),
	// 	svcInformer.Informer(),
	// })
	informerFactory.Start(ctx.Done())
	wasmInformerFactory.Start(ctx.Done())

	t.Log("Informers required for wasm manager started")
    

	if ok := k8sCache.WaitForCacheSync(ctx.Done(), wasm.deplListerSynced,wasm.svcListerSynced); !ok {
		t.Fatal("Timed out waiting for caches to sync")
	}
	
    //模拟客户端CLI创建函数（wasm文件）
	//创建一个wasm function
	funcUID, err := uuid.NewV4()
	if err != nil {
		t.Fatal(err)
	}

	funcSpec := fv1.Function{
		ObjectMeta: metav1.ObjectMeta{
			Name:      functionName,
			Namespace: defaultNamespace,
			UID:       types.UID(funcUID.String()),
		},
		Spec: fv1.FunctionSpec{
			InvokeStrategy: fv1.InvokeStrategy{
				ExecutionStrategy: fv1.ExecutionStrategy{
					ExecutorType: fv1.ExecutorTypeWasmShortS,
					MinScale:     int(1),
				},
			},
		},
	}
	

	
	//创建一个wasm的container
	container := &apiv1.Container{
		Name:  functionName,
		Image: "wasmedge/xxxx",
		Ports: []apiv1.ContainerPort{
			{
				Name:          "function",
				ContainerPort: int32(8888),
			},
		},
	}
	fnGracePeriod := int64(6 * 60)
	funcSpec.Spec.PodSpec = &apiv1.PodSpec{
		Containers:                    []apiv1.Container{*container},
		TerminationGracePeriodSeconds: &fnGracePeriod,
	}


    //模拟Controller创建CRD资源
	function, err := wasm.fissionClient.CoreV1().Functions(defaultNamespace).Create(ctx, &funcSpec, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating function failed : %s", err)
	}

	//底层真正部署函数
	// funcSvc,err:=wasm.createFunction(ctx,function)
	_,err=wasm.createShortFunction(ctx,function)
	if err != nil {
		t.Fatalf("Error creating wasm function: %s", err)
	}
	fmt.Printf("成功部署wasm函数！！!\n")
	
	objname:=wasm.getObjName(function)
	ns := wasm.namespace


	fmt.Printf("查找job。。。。\n")
	job,err:=wasm.kubernetesClient.BatchV1().Jobs(ns).Get(ctx,objname,metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Error getting wasm function job  %s", err)
	}
	fmt.Printf("wasm function job 创建成功：%s\n",job.Name)

	fmt.Printf("删除函数。。。。\n")
	//删除函数 
	err=wasm.deleteShortFunction(ctx,function)
	if err != nil {
		t.Fatalf("Error deleting short wasm function: %s", err)
	}
	fmt.Printf("删除成功！！！\n")

    //再去查看是否存在
	_,err=wasm.kubernetesClient.BatchV1().Jobs(ns).Get(ctx,objname,metav1.GetOptions{})
	if err != nil {
		fmt.Printf("查找job 不存在。。。。\n")
	}
	fmt.Printf("删除函数之后job随即被删除！！！\n")
  
}

// func TestShortFnDelete(t *testing.T){
// 	logger := loggerfactory.GetLogger()
// 	kubernetesClient := fake.NewSimpleClientset()
//     fissionClient := fClient.NewSimpleClientset()
// 	informerFactory := genInformer.NewSharedInformerFactory(fissionClient, time.Minute*30)
// 	funcInformer := informerFactory.Core().V1().Functions()
// 	// envInformer := informerFactory.Core().V1().Environments()

// 	wasmInformerFactory, err := utils.GetInformerFactoryByExecutor(kubernetesClient, fv1.ExecutorTypeWasm, time.Minute*30)
// 	if err != nil {
// 		t.Fatalf("Error creating informer factory: %s", err)
// 	}

// 	deployInformer := wasmInformerFactory.Apps().V1().Deployments()
// 	svcInformer := wasmInformerFactory.Core().V1().Services()
// 	Jobnformer := wasmInformerFactory.Batch().V1().Jobs()

// 	ctx, cancel := context.WithCancel(context.Background())
// 	defer cancel()

//     //创建wasm组件
// 	executor, err := MakeWasm(ctx, logger, fissionClient, kubernetesClient, functionNamespace,"test",funcInformer,Jobnformer ,deployInformer,svcInformer)
// 	if err != nil {
// 		t.Fatalf("new deploy manager creation failed: %s", err)
// 	}

// 	wasm := executor.(*Wasm)

//     //运行组件
// 	go wasm.Run(ctx)
// 	t.Log("New deploy manager started")

// 	// runInformers(ctx, []k8sCache.SharedIndexInformer{
// 	// 	funcInformer.Informer(),
// 	// 	deployInformer.Informer(),
// 	// 	svcInformer.Informer(),
// 	// })
// 	informerFactory.Start(ctx.Done())
// 	wasmInformerFactory.Start(ctx.Done())

// 	t.Log("Informers required for wasm manager started")
    

// 	if ok := k8sCache.WaitForCacheSync(ctx.Done(), wasm.deplListerSynced,wasm.svcListerSynced); !ok {
// 		t.Fatal("Timed out waiting for caches to sync")
// 	}
	
//     //模拟客户端CLI创建函数（wasm文件）
// 	//创建一个wasm function
// 	funcUID, err := uuid.NewV4()
// 	if err != nil {
// 		t.Fatal(err)
// 	}

// 	funcSpec := fv1.Function{
// 		ObjectMeta: metav1.ObjectMeta{
// 			Name:      functionName,
// 			Namespace: defaultNamespace,
// 			UID:       types.UID(funcUID.String()),
// 		},
// 		Spec: fv1.FunctionSpec{
// 			InvokeStrategy: fv1.InvokeStrategy{
// 				ExecutionStrategy: fv1.ExecutionStrategy{
// 					ExecutorType: fv1.ExecutorTypeWasm,
// 					MinScale:     int(1),
// 				},
// 			},
// 			Package: fv1.FunctionPackageRef{
// 				PackageRef: fv1.PackageRef{
// 					Namespace: defaultNamespace,
// 					Name: pkgName,
// 					ResourceVersion:"vtest",
// 				},

// 			},
// 		},
// 	}
	
// 	//创建一个wasm的pakage
// 	pkg := &fv1.Package{
// 		ObjectMeta: metav1.ObjectMeta{
// 			Name:      pkgName,
// 			Namespace: defaultNamespace,
// 			UID:       "83c82da2-81e9-4ebd-867e-f383e65e603f",
// 		},
// 		Spec: fv1.PackageSpec{//有Deployment archive和source archive
// 			Deployment: fv1.Archive{
// 				Type: fv1.ArchiveTypeUrl,
// 				URL:   "/hhhh/test_wasm",
// 			},
// 		},
// 	}
	
// 	//创建一个wasm的container
// 	container := &apiv1.Container{
// 		Name:  functionName,
// 		Image: pkg.Spec.Deployment.URL,
// 		Ports: []apiv1.ContainerPort{
// 			{
// 				Name:          "function",
// 				ContainerPort: int32(8888),
// 			},
// 		},
// 	}
// 	fnGracePeriod := int64(6 * 60)
// 	funcSpec.Spec.PodSpec = &apiv1.PodSpec{
// 		Containers:                    []apiv1.Container{*container},
// 		TerminationGracePeriodSeconds: &fnGracePeriod,
// 	}


//     //模拟Controller创建CRD资源
// 	_, err = wasm.fissionClient.CoreV1().Packages(defaultNamespace).Create(ctx, pkg, metav1.CreateOptions{})
// 	if err != nil {
// 		t.Fatalf("creating package failed : %s", err)
// 	}
// 	function, err := wasm.fissionClient.CoreV1().Functions(defaultNamespace).Create(ctx, &funcSpec, metav1.CreateOptions{})
// 	if err != nil {
// 		t.Fatalf("creating function failed : %s", err)
// 	}

// 	//底层真正部署函数
// 	// funcSvc,err:=wasm.createFunction(ctx,function)
// 	_,err=wasm.createFunction(ctx,function)
// 	if err != nil {
// 		t.Fatalf("Error creating wasm function: %s", err)
// 	}

// 	t.Log("成功部署wasm 函数 !")
	
// 	objname:=wasm.getObjName(function)
// 	fmt.Printf("wasm function deployment 创建成功：%s\n",objname)
// 	ns := wasm.namespace

// 	dpl,err:=wasm.kubernetesClient.AppsV1().Deployments(ns).Get(ctx,objname,metav1.GetOptions{})
// 	if err != nil {
// 		t.Fatalf("Error getting wasm function deployment  %s", err)
// 	}
// 	fmt.Printf("验证。。。。\n")

// 	fmt.Printf("wasm function deployment 创建成功：%s\n",dpl.Name)

// 	//删除函数 
// 	err=wasm.deleteFunction(ctx,function)
// 	if err != nil {
// 		t.Fatalf("Error deleting wasm function: %s", err)
// 	}
//     //再去查看是否存在
// 	dpl,err=wasm.kubernetesClient.AppsV1().Deployments(ns).Get(ctx,objname,metav1.GetOptions{})
// 	if err != nil {
// 		t.Log("Error getting wasm function deployment 说明删除成功 ")
// 	}

// }

// func TestFnUpdate(t *testing.T){
// 	logger := loggerfactory.GetLogger()
// 	kubernetesClient := fake.NewSimpleClientset()
//     fissionClient := fClient.NewSimpleClientset()
// 	informerFactory := genInformer.NewSharedInformerFactory(fissionClient, time.Minute*30)
// 	funcInformer := informerFactory.Core().V1().Functions()
// 	// envInformer := informerFactory.Core().V1().Environments()

// 	wasmInformerFactory, err := utils.GetInformerFactoryByExecutor(kubernetesClient, fv1.ExecutorTypeWasm, time.Minute*30)
// 	if err != nil {
// 		t.Fatalf("Error creating informer factory: %s", err)
// 	}

// 	deployInformer := wasmInformerFactory.Apps().V1().Deployments()
// 	svcInformer := wasmInformerFactory.Core().V1().Services()
	

// 	ctx, cancel := context.WithCancel(context.Background())
// 	defer cancel()

//     //创建wasm组件
// 	executor, err := MakeWasm(ctx, logger, fissionClient, kubernetesClient, functionNamespace,"test",funcInformer, deployInformer,svcInformer)
// 	if err != nil {
// 		t.Fatalf("new deploy manager creation failed: %s", err)
// 	}

// 	wasm := executor.(*Wasm)

//     //运行组件
// 	go wasm.Run(ctx)
// 	t.Log("New deploy manager started")

// 	// runInformers(ctx, []k8sCache.SharedIndexInformer{
// 	// 	funcInformer.Informer(),
// 	// 	deployInformer.Informer(),
// 	// 	svcInformer.Informer(),
// 	// })
// 	informerFactory.Start(ctx.Done())
// 	wasmInformerFactory.Start(ctx.Done())

// 	t.Log("Informers required for wasm manager started")
    

// 	if ok := k8sCache.WaitForCacheSync(ctx.Done(), wasm.deplListerSynced,wasm.svcListerSynced); !ok {
// 		t.Fatal("Timed out waiting for caches to sync")
// 	}
	
//     //模拟客户端CLI创建函数（wasm文件）
// 	//创建一个wasm function
// 	funcUID, err := uuid.NewV4()
// 	if err != nil {
// 		t.Fatal(err)
// 	}

// 	funcSpec := fv1.Function{
// 		ObjectMeta: metav1.ObjectMeta{
// 			Name:      functionName,
// 			Namespace: defaultNamespace,
// 			UID:       types.UID(funcUID.String()),
// 		},
// 		Spec: fv1.FunctionSpec{
// 			InvokeStrategy: fv1.InvokeStrategy{
// 				ExecutionStrategy: fv1.ExecutionStrategy{
// 					ExecutorType: fv1.ExecutorTypeWasm,
// 					MinScale:     int(1),
// 				},
// 			},
// 			Package: fv1.FunctionPackageRef{
// 				PackageRef: fv1.PackageRef{
// 					Namespace: defaultNamespace,
// 					Name: pkgName,
// 					ResourceVersion:"vtest",
// 				},

// 			},
// 		},
// 	}
	
// 	//创建一个wasm的pakage
// 	pkg := &fv1.Package{
// 		ObjectMeta: metav1.ObjectMeta{
// 			Name:      pkgName,
// 			Namespace: defaultNamespace,
// 			UID:       "83c82da2-81e9-4ebd-867e-f383e65e603f",
// 		},
// 		Spec: fv1.PackageSpec{//有Deployment archive和source archive
// 			Deployment: fv1.Archive{
// 				Type: fv1.ArchiveTypeUrl,
// 				URL:   "/hhhh/test_wasm",
// 			},
// 		},
// 	}
	
// 	//创建一个wasm的container
// 	container := &apiv1.Container{
// 		Name:  functionName,
// 		Image: pkg.Spec.Deployment.URL,
// 		Ports: []apiv1.ContainerPort{
// 			{
// 				Name:          "function",
// 				ContainerPort: int32(8888),
// 			},
// 		},
// 	}
// 	fnGracePeriod := int64(6 * 60)
// 	funcSpec.Spec.PodSpec = &apiv1.PodSpec{
// 		Containers:                    []apiv1.Container{*container},
// 		TerminationGracePeriodSeconds: &fnGracePeriod,
// 	}


//     //模拟Controller创建CRD资源
// 	_, err = wasm.fissionClient.CoreV1().Packages(defaultNamespace).Create(ctx, pkg, metav1.CreateOptions{})
// 	if err != nil {
// 		t.Fatalf("creating package failed : %s", err)
// 	}
// 	function, err := wasm.fissionClient.CoreV1().Functions(defaultNamespace).Create(ctx, &funcSpec, metav1.CreateOptions{})
// 	if err != nil {
// 		t.Fatalf("creating function failed : %s", err)
// 	}

// 	//底层真正部署函数
// 	// funcSvc,err:=wasm.createFunction(ctx,function)
// 	_,err=wasm.createFunction(ctx,function)
// 	if err != nil {
// 		t.Fatalf("Error creating wasm function: %s", err)
// 	}

// 	t.Log("成功部署wasm 函数 !")
	
// 	objname:=wasm.getObjName(function)
// 	fmt.Printf("wasm function deployment 创建成功：%s\n",objname)
// 	ns := wasm.namespace
// 	if funcSpec.ObjectMeta.Namespace != metav1.NamespaceDefault {
// 		ns = funcSpec.ObjectMeta.Namespace
// 	}

// 	dpl,err:=wasm.kubernetesClient.AppsV1().Deployments(ns).Get(ctx,objname,metav1.GetOptions{})
// 	if err != nil {
// 		t.Fatalf("Error getting wasm function deployment  %s", err)
// 	}
// 	fmt.Printf("验证。。。。\n")

// 	fmt.Printf("wasm function deployment 创建成功：%s\n",dpl.Name)

// 	//更新函数 

// 	//构造新的函数
// 	newFunSpec:=&fv1.Function{
// 		ObjectMeta: metav1.ObjectMeta{
// 			Name:      newFunctionName,
// 			Namespace: defaultNamespace,
// 			UID:       types.UID(funcUID.String()),
// 		},
// 		Spec: fv1.FunctionSpec{
// 			InvokeStrategy: fv1.InvokeStrategy{
// 				ExecutionStrategy: fv1.ExecutionStrategy{
// 					ExecutorType: fv1.ExecutorTypeWasm,
// 					MinScale:     int(1),
// 				},
// 			},
// 			Package: fv1.FunctionPackageRef{
// 				PackageRef: fv1.PackageRef{
// 					Namespace: defaultNamespace,
// 					Name: pkgName,
// 					ResourceVersion:"vtest",
// 				},

// 			},
// 		},
// 	}
// 	//创建一个新的wasm的pakage
// 	newPkg := &fv1.Package{
// 		ObjectMeta: metav1.ObjectMeta{
// 			Name:      pkgName,
// 			Namespace: defaultNamespace,
// 			UID:       "88c82da2-81e9-4ebd-867e-f383e65e603f",
// 		},
// 		Spec: fv1.PackageSpec{//有Deployment archive和source archive
// 			Deployment: fv1.Archive{
// 				Type: fv1.ArchiveTypeUrl,
// 				URL:   "/hhhh/test_wasm_update",
// 			},
// 		},
// 	}
//     //创建一个wasm的container
// 	newContainer := &apiv1.Container{
// 		Name:  newFunctionName,
// 		Image: newPkg.Spec.Deployment.URL,
// 		Ports: []apiv1.ContainerPort{
// 			{
// 				Name:          "newFunction",
// 				ContainerPort: int32(8866),
// 			},
// 		},
// 	}
// 	newFunSpec.Spec.PodSpec = &apiv1.PodSpec{
// 		Containers:                    []apiv1.Container{*newContainer},
// 		TerminationGracePeriodSeconds: &fnGracePeriod,
// 	}
// 	_,err=wasm.createFunction(ctx,newFunSpec)
// 	if err != nil {
// 		t.Fatalf("Error creating wasm newFunSpec: %s", err)
// 	}

// 	t.Log("成功部署newFunSpec wasm 函数 !")


// 	//更新函数了
// 	err=wasm.updateFunction(ctx,function,newFunSpec)
// 	if err != nil {
// 		t.Fatalf("Error deleting wasm function: %s", err)
// 	}
//     //再去查看是否存在
// 	dpl,err=wasm.kubernetesClient.AppsV1().Deployments(ns).Get(ctx,objname,metav1.GetOptions{})
// 	if err != nil {
// 		t.Log("不在旧的objname里面 ")
// 	}
// 	objname=wasm.getObjName(newFunSpec)
// 	fmt.Printf("newwasm function deployment 创建成功：%s\n",objname)
// 	ns = wasm.namespace
// 	if newFunSpec.ObjectMeta.Namespace != metav1.NamespaceDefault {
// 		ns = newFunSpec.ObjectMeta.Namespace
// 	}
// 	dpl,err=wasm.kubernetesClient.AppsV1().Deployments(ns).Get(ctx,objname,metav1.GetOptions{})
// 	if err != nil {
// 		t.Log("也不在在新的objname里面 说明更新不成功 ")
// 	}

// 	fmt.Printf("验证。。。。\n")

// 	fmt.Printf("wasm function deployment 更新成功：%s\n",dpl.Name)

// }

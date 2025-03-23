package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/kubectl/pkg/scheme"
)

const (
	scanInterval        = 1 * time.Second
	protectedNamespaces = "kube-system,kube-public,monitoring"
	criticalLabels      = "app.kubernetes.io/component=monitoring"
)

func main() {
	// 设置Kubernetes集群环境变量
	// os.Setenv("KUBERNETES_SERVICE_HOST", "https://kubernetes.docker.internal")
	// os.Setenv("KUBERNETES_SERVICE_PORT", "6443")
	// // 初始化Kubernetes客户端
	// config, err := rest.InClusterConfig()
	// if err != nil {
	// 	log.Fatalf("无法获取集群配置: %v", err)
	// }

	// 获取本地Kubernetes配置文件路径
	kubeconfig := filepath.Join(os.Getenv("HOME"), ".kube", "config")

	// 使用本地配置文件初始化Kubernetes客户端
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		log.Fatalf("无法获取本地配置: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("创建客户端失败: %v", err)
	}

	log.Println("启动Pod扫描器...")
	ticker := time.NewTicker(scanInterval)
	defer ticker.Stop()

	for range ticker.C {
		log.Println("开始新一轮扫描")
		if err := scanCluster(clientset, config); err != nil {
			log.Printf("扫描过程中发生错误: %v", err)
		}
	}
}

// 主扫描逻辑
func scanCluster(clientset *kubernetes.Clientset, config *rest.Config) error {
	namespaces, err := getTargetNamespaces(clientset)
	if err != nil {
		return err
	}

	for _, ns := range namespaces {
		log.Printf("扫描命名空间: %s", ns)
		pods, err := clientset.CoreV1().Pods(ns).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			log.Printf("无法列出%s中的Pod: %v", ns, err)
			continue
		}

		for _, pod := range pods.Items {
			if shouldSkipPod(&pod) {
				log.Printf("跳过Pod: %s/%s", pod.Namespace, pod.Name)
				continue
			}

			log.Printf("检查Pod: %s/%s", pod.Namespace, pod.Name)
			if isJavaWithoutPinpoint(clientset, config, &pod) {
				log.Printf("发现不合规Pod: %s/%s", pod.Namespace, pod.Name)
				if err := handleNonCompliantPod(clientset, &pod); err != nil {
					log.Printf("处理失败: %v", err)
				}
			}
		}
	}
	return nil
}

// 获取需要扫描的命名空间
func getTargetNamespaces(clientset *kubernetes.Clientset) ([]string, error) {
	nsList, err := clientset.CoreV1().Namespaces().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	var targets []string
	for _, ns := range nsList.Items {
		if !isProtectedNamespace(ns.Name) {
			targets = append(targets, ns.Name)
		}
	}
	//过滤出_test的命名空间
	var targetList []string
	for _, ns := range targets {
		if strings.Contains(ns, "_test") {
			targetList = append(targetList, ns)
		}
	}

	return targetList, nil
}

// 判断是否受保护命名空间
func isProtectedNamespace(name string) bool {
	protected := strings.Split(protectedNamespaces, ",")
	for _, ns := range protected {
		if strings.EqualFold(ns, name) {
			return true
		}
	}
	return false
}

// 判断是否需要跳过Pod
func shouldSkipPod(pod *corev1.Pod) bool {
	// 跳过已标记删除的Pod
	if pod.DeletionTimestamp != nil {
		return true
	}

	// 跳过系统组件
	if hasCriticalLabels(pod.Labels) {
		return true
	}

	// 仅处理Deployment管理的Pod
	return !isDeploymentPod(pod)
}

// 检查关键标签
func hasCriticalLabels(labelMap map[string]string) bool {
	requiredLabels := strings.Split(criticalLabels, ",")
	for _, l := range requiredLabels {
		parts := strings.Split(l, "=")
		if len(parts) != 2 {
			continue
		}
		if val, exists := labelMap[parts[0]]; exists && val == parts[1] {
			return true
		}
	}
	return false
}

// 判断是否为Deployment管理的Pod
func isDeploymentPod(pod *corev1.Pod) bool {
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "ReplicaSet" {
			return true
		}
	}
	return false
}

// 主容器检测逻辑
func getMainContainer(pod *corev1.Pod) string {
	// 1. 检查注解定义的主容器
	if name, ok := pod.Annotations["app.kubernetes.io/main-container"]; ok {
		for _, c := range pod.Spec.Containers {
			if c.Name == name {
				return c.Name
			}
		}
	}

	// 2. 查找名称包含main的容器
	for _, c := range pod.Spec.Containers {
		if strings.Contains(strings.ToLower(c.Name), "main") {
			return c.Name
		}
	}

	// 3. 默认返回第一个容器
	if len(pod.Spec.Containers) > 0 {
		return pod.Spec.Containers[0].Name
	}
	return ""
}

// 检测Java进程是否缺少Pinpoint
func isJavaWithoutPinpoint(clientset *kubernetes.Clientset, config *rest.Config, pod *corev1.Pod) bool {
	mainContainer := getMainContainer(pod)
	if mainContainer == "" {
		log.Printf("Pod %s/%s 没有有效容器", pod.Namespace, pod.Name)
		return false
	}

	javaDetected := checkJavaProcess(clientset, config, pod, mainContainer)
	pinpointDetected := checkPinpointAgent(clientset, config, pod, mainContainer)

	log.Printf("检测结果 - Java: %t, Pinpoint: %t", javaDetected, pinpointDetected)
	return javaDetected && !pinpointDetected
}

// 检查Java进程
func checkJavaProcess(clientset *kubernetes.Clientset, config *rest.Config, pod *corev1.Pod, container string) bool {
	//cmd := []string{"sh", "-c", "ps -ef | grep -v grep | grep java"}
	cmd := []string{"sh", "-c", "ps -ef | grep -v grep | grep infinity"}
	_, err := execCommand(clientset, config, pod, container, cmd)
	return err == nil
}

// 检查Pinpoint代理
func checkPinpointAgent(clientset *kubernetes.Clientset, config *rest.Config, pod *corev1.Pod, container string) bool {
	cmd := []string{"sh", "-c", "ps -ef | grep -v grep | grep pinpoint"}
	_, err := execCommand(clientset, config, pod, container, cmd)
	return err == nil
}

// 执行容器命令
func execCommand(clientset *kubernetes.Clientset, config *rest.Config, pod *corev1.Pod, container string, cmd []string) (string, error) {
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod.Name).
		Namespace(pod.Namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		return "", err
	}

	var stdout, stderr bytes.Buffer
	err = executor.Stream(remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})

	if err != nil {
		return "", fmt.Errorf("执行错误: %s | %s", stderr.String(), err)
	}
	return stdout.String(), nil
}

// 处理不合规Pod
func handleNonCompliantPod(clientset *kubernetes.Clientset, pod *corev1.Pod) error {
	deployment, err := findParentDeployment(clientset, pod)
	if err != nil {
		return fmt.Errorf("找不到关联Deployment: %v", err)
	}

	// 检查当前副本数
	if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas == 0 {
		log.Printf("Deployment %s/%s 已缩容", deployment.Namespace, deployment.Name)
		return nil
	}

	log.Printf("正在缩容并标记 Deployment: %s/%s", deployment.Namespace, deployment.Name)
	return scaleDeployment(clientset, deployment, 0)
}

// 查找父级Deployment
func findParentDeployment(clientset *kubernetes.Clientset, pod *corev1.Pod) (*appsv1.Deployment, error) {
	rsName := ""
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "ReplicaSet" {
			rsName = ref.Name
			break
		}
	}

	if rsName == "" {
		return nil, fmt.Errorf("Pod不是由Deployment管理")
	}

	rs, err := clientset.AppsV1().ReplicaSets(pod.Namespace).Get(context.TODO(), rsName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	for _, ref := range rs.OwnerReferences {
		if ref.Kind == "Deployment" {
			return clientset.AppsV1().Deployments(pod.Namespace).Get(
				context.TODO(),
				ref.Name,
				metav1.GetOptions{},
			)
		}
	}
	return nil, fmt.Errorf("未找到父级Deployment")
}

// 缩容Deployment
func scaleDeployment(clientset *kubernetes.Clientset, deployment *appsv1.Deployment, replicas int32) error {
	now := time.Now().Format("20060102T150405Z07")
	now = strings.ReplaceAll(now, ":", "-")
	now = strings.ReplaceAll(now, "+", "-")
	patch := fmt.Sprintf(`{
		"metadata": {
			"labels": {
				"pinpoint-scanner/status": "killed by pod-scanner",
				"pinpoint-scanner/last-terminated": "%s"
			}
		},
		"spec": {
			"replicas": %d
		}
	}`, now, replicas)

	_, err := clientset.AppsV1().Deployments(deployment.Namespace).Patch(
		context.TODO(),
		deployment.Name,
		types.StrategicMergePatchType,
		[]byte(patch),
		metav1.PatchOptions{},
	)
	return err
}

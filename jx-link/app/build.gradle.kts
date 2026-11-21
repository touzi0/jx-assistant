plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

android {
    namespace = "com.jianxiang.link"
    compileSdk = 35

    defaultConfig {
        applicationId = "com.jianxiang.link"
        minSdk = 26          // 与主壳一致（Android 8.0 起）
        targetSdk = 34       // 无 W^X 需求，锚新一些；35 会强制 edge-to-edge，34 免处理
        versionCode = 2026112101
        versionName = "20261121-1"
    }

    signingConfigs {
        create("release") {
            // 开发自用签名：build.sh 首次生成 app/debug.keystore
            val ksPath = file("debug.keystore")
            if (ksPath.exists()) {
                storeFile = ksPath
                storePassword = "jianxiang"
                keyAlias = "jianxiang"
                keyPassword = "jianxiang"
            }
        }
    }

    buildTypes {
        release {
            isMinifyEnabled = false
            if (file("debug.keystore").exists()) {
                signingConfig = signingConfigs.getByName("release")
            }
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    kotlinOptions {
        jvmTarget = "17"
    }
}

dependencies {
    implementation("androidx.core:core-ktx:1.15.0")
    // ComponentActivity（LifecycleOwner，CameraX 绑定生命周期用）
    implementation("androidx.activity:activity:1.9.3")
    // 多 Profile（每台电脑独立缓存域）+ ProfileStore（按设备清缓存）
    implementation("androidx.webkit:webkit:1.12.1")
    // 扫码：CameraX 预览/分析 + ZXing 解码（无 ML Kit，不依赖 GMS，体积小）
    implementation("androidx.camera:camera-core:1.3.4")
    implementation("androidx.camera:camera-camera2:1.3.4")
    implementation("androidx.camera:camera-lifecycle:1.3.4")
    implementation("androidx.camera:camera-view:1.3.4")
    implementation("com.google.zxing:core:3.5.3")
}

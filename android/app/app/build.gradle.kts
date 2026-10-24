import java.util.Properties

plugins {
    id("com.android.application")
}

// 签名信息放在 keystore.properties（不入库），没有它就只出未签名的包。
val keystoreProps = Properties().apply {
    val f = rootProject.file("keystore.properties")
    if (f.exists()) f.inputStream().use { load(it) }
}
val hasSigning = keystoreProps.getProperty("storeFile") != null

android {
    namespace = "com.codexmobile"
    compileSdk = 35

    defaultConfig {
        applicationId = "io.github.jincv0.suixing"
        minSdk = 26
        targetSdk = 35
        versionCode = 1
        versionName = "1.0.0"
    }

    signingConfigs {
        if (hasSigning) {
            create("release") {
                storeFile = rootProject.file(keystoreProps.getProperty("storeFile"))
                storePassword = keystoreProps.getProperty("storePassword")
                keyAlias = keystoreProps.getProperty("keyAlias")
                keyPassword = keystoreProps.getProperty("keyPassword")
            }
        }
    }

    buildTypes {
        release {
            isMinifyEnabled = false
            if (hasSigning) signingConfig = signingConfigs.getByName("release")
        }
        debug {
            isMinifyEnabled = false
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    packaging {
        jniLibs {
            useLegacyPackaging = false
        }
    }

    // BuildConfig 里要用 VERSION_CODE / VERSION_NAME 告诉网页当前版本
    buildFeatures {
        buildConfig = true
    }
}

dependencies {
    // 扫二维码：直接用成熟库，省得自己写相机预览和解码。
    // 它只依赖 zxing core，不需要 androidx，所以整个工程一个 androidX 组件都不引。
    implementation("com.journeyapps:zxing-android-embedded:4.3.0")
    implementation("com.google.zxing:core:3.5.3")

    // zxing-android-embedded 的 POM 里漏声明了 androidx.core，
    // 但它内部（CaptureManager.openCameraWithPermission）真的在用
    // androidx.core.content.ContextCompat。不补这一行，扫码界面一打开就是
    // NoClassDefFoundError。只补 core，不引 appcompat/emoji2/startup 那一串。
    implementation("androidx.core:core:1.13.1")
}

// androidx.core 会带进 kotlin-stdlib，历史上和 jdk7/jdk8 分包版本撞过重复类。
// 1.8 起 jdk7/jdk8 已并入主包，统一到同一版本即可。
configurations.all {
    resolutionStrategy.eachDependency {
        if (requested.group == "org.jetbrains.kotlin") {
            useVersion("1.8.22")
        }
    }
}

plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
    id("app.cash.licensee")
}

android {
    namespace = "dev.wechatcopilot.companion"
    compileSdk = 35

    defaultConfig {
        applicationId = "dev.wechatcopilot.companion"
        minSdk = 30
        targetSdk = 35
        versionCode = 1
        versionName = "0.1.0"
    }

    buildTypes {
        release {
            isMinifyEnabled = true
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"), "proguard-rules.pro")
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    kotlinOptions {
        jvmTarget = "17"
    }
    testOptions {
        unitTests.isReturnDefaultValues = true
    }
}

dependencies {
    testImplementation(kotlin("test"))
}

licensee {
    allow("Apache-2.0")
    allow("BSD-2-Clause")
    allow("BSD-3-Clause")
    allow("ISC")
    allow("MIT")
    allow("MPL-2.0")
}

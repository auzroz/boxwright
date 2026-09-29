require "json"

package = JSON.parse(File.read(File.join(__dir__, "package.json")))

# Modelled on react-native-mmkv's NitroMmkv.podspec (Nitro 0.37). A local pod:
# the app depends on this directory as "file:./modules/boxwright-depth" and
# React Native autolinking finds this file through node_modules, so the app's
# project.pbxproj never needs editing for it.
Pod::Spec.new do |s|
  s.name         = "BoxwrightDepth"
  s.version      = package["version"]
  s.summary      = package["description"]
  s.homepage     = package["homepage"]
  s.license      = package["license"]
  s.authors      = package["author"]

  s.platforms    = { :ios => min_ios_version_supported }
  s.source       = { :git => "https://github.com/auzroz/boxwright.git", :tag => "v#{s.version}" }
  # The app target's own setting; see ios/Boxwright.xcodeproj.
  s.swift_version = "5.0"

  s.source_files = [
    # Implementation (Swift)
    "ios/**/*.{swift}",
  ]

  # ARKit and RealityKit are system frameworks on every iPhone this app
  # supports; linking them does not make LiDAR a requirement to install
  # (that would be UIRequiredDeviceCapabilities, which check-ios-config.sh
  # keeps free of it).
  s.frameworks = "ARKit", "RealityKit", "CoreImage", "ImageIO", "UniformTypeIdentifiers"

  load 'nitrogen/generated/ios/BoxwrightDepth+autolinking.rb'
  add_nitrogen_files(s)

  s.dependency 'React-jsi'
  s.dependency 'React-callinvoker'
  install_modules_dependencies(s)
end

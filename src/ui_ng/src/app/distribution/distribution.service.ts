import { Injectable } from '@angular/core';
import { Http } from '@angular/http';
import { Observable } from 'rxjs/Observable';
import "rxjs/add/observable/of";
import { HTTP_JSON_OPTIONS, buildHttpRequestOptions } from "../shared/shared.utils";
import { RequestQueryParams } from 'harbor-ui';
import { DistributionHistory } from './distribution-history';
import { DistributionProvider, ProviderInstance, AuthMode } from './distribution-provider';

@Injectable()
export class DistributionService {

  constructor(private http: Http) { }

  getDistributionHistories(params?: RequestQueryParams): Observable<DistributionHistory[]> {
    let mockData: DistributionHistory[] = [
      {
        image: "library/redis:latest",
        timestamp: new Date(),
        status: "PENDING",
        provider: "Dragonfly",
        instance: "uuid"
      }
    ];
    return Observable.of(mockData);
  }

  getProviderInstances(params?: RequestQueryParams): Observable<ProviderInstance[]> {
    let mockProvider: DistributionProvider = {
      name: "dragonfly",
      version: "0.10.1",
      icon: "https://raw.githubusercontent.com/alibaba/Dragonfly/master/docs/images/logo.png",
      source: "https://github.com/alibaba/Dragonfly",
      maintainers: ["szou@vmware.com"],
      authMode: AuthMode.BASIC
    };

    let authData: Map<string, string> = new Map<string, string>();
    authData["username"] = "admin";
    authData["password"] = "Passw0rd";
    let mockData: ProviderInstance[] = [{
      ID: "mock_id_1",
      name: "mock instance",
      endpoint: "https://localhost:9090",
      status: "Healthy",
      enabled: true,
      setupTimestamp: new Date(),
      provider: mockProvider,
      authorization: {
        authMode: AuthMode.BASIC,
        data: authData
      }
    }];

    return Observable.of(mockData)
  }

  getProviderDrivers(params?: RequestQueryParams): Observable<DistributionProvider[]> {
    let mockProviders: DistributionProvider[] = [
      {
        name: "dragonfly",
        version: "0.10.1",
        icon: "https://raw.githubusercontent.com/alibaba/Dragonfly/master/docs/images/logo.png",
        source: "https://github.com/alibaba/Dragonfly",
        maintainers: ["szou@vmware.com"],
        authMode: AuthMode.BASIC
      }
    ];

    return Observable.of(mockProviders);
  }

}